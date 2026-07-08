use super::ProxyMode;
use anyhow::{Result, anyhow};
use policy_evaluator::{
    callback_handler::CallbackHandlerBuilder,
    callback_requests::{CallbackRequest, CallbackRequestType, CallbackResponse},
    kube,
    policy_fetcher::{sigstore::trust::sigstore::SigstoreTrustRoot, sources::Sources},
};
use serde::{Deserialize, Serialize};
use std::{
    fs::File,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};
use tokio::sync::{mpsc, oneshot};
use tracing::{error, info, warn};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type")]
enum Response {
    Success { payload: String },
    Error { message: String },
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
struct Exchange {
    pub request: String,
    pub response: Response,
}

/// Mirrors `Response`, but stores the success payload as bytes instead of a
/// `String`. Used by `ParsedExchange` so that the payload is decoded once,
/// at session-load time, instead of on every replayed request.
///
/// Note: this does not make replay fully allocation-free. Each replayed
/// call must still produce a fresh, owned `Vec<u8>`, because
/// `CallbackResponse::payload` (defined in the `policy-evaluator` crate) is
/// an owned `Vec<u8>`.
enum ParsedResponse {
    Success { payload: Vec<u8> },
    Error { message: String },
}

impl From<Response> for ParsedResponse {
    fn from(response: Response) -> Self {
        match response {
            Response::Success { payload } => ParsedResponse::Success {
                payload: payload.into_bytes(),
            },
            Response::Error { message } => ParsedResponse::Error { message },
        }
    }
}

/// A recorded exchange, with the request already deserialized into a
/// `CallbackRequestType`. Deserializing once at load time (instead of on
/// every replayed request) matters because `kwctl bench` replays the same
/// session thousands of times.
struct ParsedExchange {
    request: CallbackRequestType,
    response: ParsedResponse,
}

/// A monotonically-advancing cursor into a replay session, shared between
/// `CallbackHandlerProxy` and any `ReplayResetHandle`s that need to rewind
/// it (e.g. `kwctl bench`, before every benchmark iteration).
#[derive(Default)]
struct ReplayCursor(AtomicUsize);

impl ReplayCursor {
    /// Returns the current index and atomically advances the cursor by one.
    fn advance(&self) -> usize {
        self.0.fetch_add(1, Ordering::SeqCst)
    }

    /// Resets the cursor back to the first recorded exchange.
    fn reset(&self) {
        self.0.store(0, Ordering::SeqCst);
    }

    /// Best-effort peek at the current value, for diagnostics only (e.g.
    /// warning that not all exchanges were replayed at shutdown). Must not
    /// be relied upon for cross-thread visibility guarantees.
    fn peek(&self) -> usize {
        self.0.load(Ordering::Relaxed)
    }
}

/// A cheap, clonable handle that allows resetting a replay session back to
/// its first recorded exchange. Used by `kwctl bench`, which reuses a single
/// `Evaluator` (and therefore a single replay session) across many
/// evaluations: a session recorded from one evaluation can be replayed
/// again for every benchmark iteration by resetting the cursor beforehand.
///
/// A recorded session is expected to contain exactly the exchanges of a
/// single evaluation. Resetting mid-session (i.e. before all of its
/// exchanges have been consumed) is supported and simply restarts matching
/// from the first exchange.
#[derive(Clone)]
pub(crate) struct ReplayResetHandle(Arc<ReplayCursor>);

impl ReplayResetHandle {
    pub(crate) fn reset(&self) {
        self.0.reset();
    }
}

/// A proxy against a `policy_evaluator::CallbackHandler`
/// Can record guest requests, save them to file and reply them back
pub(crate) struct CallbackHandlerProxy {
    sources: Option<Sources>,
    sigstore_trust_root: Option<Arc<SigstoreTrustRoot>>,
    kube_client: Option<kube::Client>,
    mode: ProxyMode,

    /// List of exchanges that happen between the policy and the
    /// host. This is populated only when the proxy is ran in
    /// `record` mode.
    ///
    /// Important, something can go wrong while acting as a proxy,
    /// hence we store `Result` objects inside of this vector.
    /// We deal with failures later on, when writing the session
    /// file.
    recorded_exchanges: Vec<Result<Exchange>>,

    /// Cursor into the list of recorded exchanges being replayed. Shared
    /// with the `ReplayResetHandle` handed out to callers that need to
    /// rewind the session (e.g. `kwctl bench`). Only used in replay mode.
    replay_cursor: Arc<ReplayCursor>,

    rx: mpsc::Receiver<CallbackRequest>,
    tx: mpsc::Sender<CallbackRequest>,
    shutdown_channel: oneshot::Receiver<()>,
}

impl CallbackHandlerProxy {
    pub async fn new(
        mode: &ProxyMode,
        shutdown_channel: oneshot::Receiver<()>,
        sources: Option<Sources>,
        sigstore_trust_root: Option<Arc<SigstoreTrustRoot>>,
        kube_client: Option<kube::Client>,
    ) -> Result<CallbackHandlerProxy> {
        // the channels used to interact with this callback handler.
        // consumers of these channels think they are interacting
        // with a regular `policy_evaluator` CallbackHandler, but
        // they are just talking with this proxy instance
        let (tx, rx) = mpsc::channel(200);

        Ok(Self {
            mode: mode.to_owned(),
            tx,
            rx,
            shutdown_channel,
            sources,
            sigstore_trust_root,
            kube_client,
            recorded_exchanges: vec![],
            replay_cursor: Arc::new(ReplayCursor::default()),
        })
    }

    pub fn sender_channel(&self) -> mpsc::Sender<CallbackRequest> {
        self.tx.clone()
    }

    /// Returns a handle that can be used to reset the replay session back
    /// to its first recorded exchange. Only meaningful (returns `Some`) when
    /// the proxy is running in replay mode.
    pub fn replay_reset_handle(&self) -> Option<ReplayResetHandle> {
        match &self.mode {
            ProxyMode::Replay { .. } => Some(ReplayResetHandle(self.replay_cursor.clone())),
            ProxyMode::Record { .. } => None,
        }
    }

    fn record_exchange(
        &mut self,
        request: Result<String>,
        response: std::result::Result<&CallbackResponse, &anyhow::Error>,
    ) {
        let exchange: Result<Exchange> = request
            .map(|req_str| {
                // the request is `Ok`. We have to convert the
                // response payload now
                response.map_or_else(
                    |resp_err| {
                        // host replied with an error (like trying to obtain the
                        // sigstore signature of an unsigned image). This is fine
                        Ok(Exchange {
                            request: req_str.clone(),
                            response: Response::Error {
                                message: resp_err.to_string(),
                            },
                        })
                    },
                    |resp| {
                        Ok(Exchange {
                            request: req_str.clone(),
                            response: Response::Success {
                                payload: String::from_utf8(resp.payload.clone()).map_err(|e| {
                                    anyhow!("cannot convert response payload to utf8: {}", e)
                                })?,
                            },
                        })
                    },
                )
            })
            .and_then(|exchange| {
                // the previous step returns a Result<Result<Exchange>>
                // because something can go wrong while converting the Response
                // payload (a Vec<u8>) to a UTF8 string.
                // This converts a Ok(Result<Exchange>) into a Result<Exchange>.
                // The conversion error would not be discarded.
                //
                // Note: we do this conversion because we want the final session
                // yaml file to be human readable. Shoving a Vec<u8> in there
                // would not help
                exchange
            });

        self.recorded_exchanges.push(exchange);
    }

    /// Write all the captured exchange messages to a file
    /// An error message is print to the stderr if there was some
    /// recording error
    fn dump_records(&self, destination: &PathBuf) {
        let errors: Vec<&anyhow::Error> = self
            .recorded_exchanges
            .iter()
            .filter_map(|exchange| exchange.as_ref().err())
            .collect();

        if !errors.is_empty() {
            error!(errors = ?errors, "Cannot record communication between host and policy, something went wrong while capturing the exchange");
        } else {
            let exchanges: Vec<&Exchange> = self
                .recorded_exchanges
                .iter()
                .filter_map(|exchange| exchange.as_ref().ok())
                .collect();
            match File::create(destination) {
                Err(e) => error!(e = ?e, ?destination, "Cannot save context aware session to file"),
                Ok(file) => match serde_yaml::to_writer(file, &exchanges) {
                    Ok(_) => info!(?destination, "Context aware session saved to file"),
                    Err(e) => error!(error = ?e, "Cannot save context aware session to file"),
                },
            }
        }
    }

    pub async fn loop_eval(&mut self) {
        match &self.mode {
            ProxyMode::Record { destination: _ } => self.loop_eval_recoder().await,
            ProxyMode::Replay { source: _ } => self.loop_eval_replay().await,
        }
    }

    /// The code used by the handler when running in `replay` mode
    async fn loop_eval_replay(&mut self) {
        // Note: in some cases we use `expect` here to panic at runtime.
        // We want the execution to be aborted if something
        // goes wrong here when dealing with channel message passing,
        // there's no nice way to handle errors here.

        let exchanges: Vec<ParsedExchange> = if let ProxyMode::Replay { source } = &self.mode {
            let file = File::open(source).unwrap_or_else(|_| {
                panic!("Cannot open host capabilities interactions file {source:?}")
            });
            let exchanges: Vec<Exchange> = serde_yaml::from_reader(file)
                .unwrap_or_else(|_| panic!("cannot deserialize contents of {source:?}"));
            exchanges
                .into_iter()
                .map(|exchange| {
                    let request: CallbackRequestType = serde_yaml::from_str(&exchange.request)
                        .expect("Cannot deserialize recorded request into `CallbackRequestType`");
                    ParsedExchange {
                        request,
                        response: exchange.response.into(),
                    }
                })
                .collect()
        } else {
            // this should never happen
            unreachable!()
        };

        loop {
            tokio::select! {
                // place the shutdown check before the message evaluation,
                // as recommended by tokio's documentation about select!
                _ = &mut self.shutdown_channel => {
                    let cursor = self.replay_cursor.peek();
                    if cursor < exchanges.len() {
                        warn!(
                            replayed = cursor,
                            total = exchanges.len(),
                            "Some of the recorded exchanges have not been replayed"
                        );
                    }
                    return;
                },
                maybe_req = self.rx.recv() => {
                    if let Some(req) = maybe_req {
                        let response = Self::produce_recorded_response(&req, &exchanges, &self.replay_cursor);

                        req.response_channel.send(response).expect("Cannot send back response to policy");
                    }
                }
            }
        }
    }

    fn produce_recorded_response(
        req: &CallbackRequest,
        exchanges: &[ParsedExchange],
        cursor: &ReplayCursor,
    ) -> Result<CallbackResponse> {
        let index = cursor.advance();
        match exchanges.get(index) {
            None => Err(anyhow!(
                "the list of recorded responses is empty or has been exhausted"
            )),
            Some(exchange) => {
                if exchange.request == req.request {
                    match &exchange.response {
                        ParsedResponse::Success { payload } => Ok(CallbackResponse {
                            payload: payload.clone(),
                        }),
                        ParsedResponse::Error { message } => Err(anyhow!("{message}")),
                    }
                } else {
                    Err(anyhow!(
                        "Replay error: unexpected request. Was expecting {:?}, got {:?} instead",
                        exchange.request,
                        req.request
                    ))
                }
            }
        }
    }

    /// The code used by the handler when running in `record` mode
    async fn loop_eval_recoder(&mut self) {
        // This is a channel used to stop the tokio task that is run
        // inside of the CallbackHandler
        let (callback_handler_shutdown_channel_tx, callback_handler_shutdown_channel_rx) =
            oneshot::channel();

        // Build the real CallbackHandler
        let mut callback_handler_builder =
            CallbackHandlerBuilder::new(callback_handler_shutdown_channel_rx)
                .registry_config(self.sources.clone())
                .trust_root(self.sigstore_trust_root.clone());
        if let Some(kc) = &self.kube_client {
            callback_handler_builder = callback_handler_builder.kube_client(kc.to_owned());
        }

        let mut callback_handler = callback_handler_builder
            .build()
            .await
            .expect("cannot build callback handler");
        let callback_handler_sender = callback_handler.sender_channel();

        // Spawn the tokio task used by the real CallbackHandler
        tokio::spawn(async move {
            callback_handler.loop_eval().await;
        });

        // loop of the proxy handler
        loop {
            tokio::select! {
                // place the shutdown check before the message evaluation,
                // as recommended by tokio's documentation about select!
                _ = &mut self.shutdown_channel => {
                    match &self.mode {
                        ProxyMode::Record{destination} =>  self.dump_records(destination),
                        _ => unreachable!()
                    }
                    if let Err(e) = callback_handler_shutdown_channel_tx.send(()) {
                        error!(error = ?e, "Cannot shutdown the real callback_handler");
                    }
                    return;
                },
                maybe_req = self.rx.recv() => {
                    // Note: in some cases we use `expect` here to panic at runtime.
                    // We want the execution to be aborted if something
                    // goes wrong here when dealing with channel message passing,
                    // there's no nice way to handle errors here.

                    if let Some(req) = maybe_req {
                        let request = serde_yaml::to_string(&req.request)
                            .map_err(|e| {
                                // the recording is compromised, but we will
                                // not panic here. We record the error and keep
                                // going with the policy execution.
                                // We will inform the user once policy execution
                                // is done and the session file is created.
                                // See `dump_records` method
                                anyhow!("cannot convert request to yaml: {}", e)
                            });

                        // Create a CallbackRequest object based on the incoming
                        // request. This is sent to the real CallbackHandler,
                        // we have to provide a different `response_channel`
                        // because we want to intercept the response
                        let (response_tx, response_rx) = oneshot::channel::<Result<CallbackResponse>>();
                        let proxy_req = CallbackRequest {
                            request: req.request,
                            response_channel: response_tx,
                        };

                        // forward the message to the real CallbackHandler,
                        // here we panic if the message cannot be sent. There's
                        // no purpose in going forward if the communication
                        // with the real CallbackHandler doesn't work
                        callback_handler_sender
                            .send(proxy_req)
                            .await
                            .expect("cannot forward request to real callback handler");

                        // same here, if we cannot get a response from the
                        // real CallbackHandler there's no reason to keep
                        // going. We can interrupt the execution if something
                        // goes wrong.
                        let response = response_rx
                            .await
                            .expect("failure while waiting for response from real callback_handler");

                        self.record_exchange(request, response.as_ref());

                        // Send back the response to the policy. Also in this
                        // case there's no nice way to recover from this error.
                        // We can interrupt the execution if something goes wrong.
                        req.response_channel
                            .send(response)
                            .expect("Cannot send back response to policy");
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parsed_exchange(request: CallbackRequestType, response: Response) -> ParsedExchange {
        ParsedExchange {
            request,
            response: response.into(),
        }
    }

    fn dummy_request(request: CallbackRequestType) -> CallbackRequest {
        let (response_tx, _) = oneshot::channel::<Result<CallbackResponse>>();
        CallbackRequest {
            request,
            response_channel: response_tx,
        }
    }

    // `CallbackRequestType` does not derive `Clone`, so tests that need the
    // same request value more than once just build it again via this helper.
    fn busybox_manifest_digest_request() -> CallbackRequestType {
        CallbackRequestType::OciManifestDigest {
            image: "busybox".to_string(),
        }
    }

    #[test]
    fn record_response_no_more_records() {
        let exchanges: Vec<ParsedExchange> = vec![];
        let cursor = ReplayCursor::default();

        let request = dummy_request(CallbackRequestType::DNSLookupHost {
            host: "kubewarden.io".to_string(),
        });

        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor);
        assert!(response.is_err());
        let err = response.unwrap_err();

        // we cannot return specialized errors because of the waPC contract
        // hence we have to unfortunately look at the error string
        assert!(err.to_string().as_str().contains("empty"));
    }

    #[test]
    fn record_response_unexpected_request() {
        let expected_exchange = parsed_exchange(
            CallbackRequestType::OciManifestDigest {
                image: "busybox".to_string(),
            },
            Response::Success {
                payload: "not relevant".to_string(),
            },
        );

        let exchanges = vec![expected_exchange];
        let cursor = ReplayCursor::default();

        let request = dummy_request(CallbackRequestType::DNSLookupHost {
            host: "kubewarden.io".to_string(),
        });

        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor);
        assert!(response.is_err());
        let err = response.unwrap_err();

        // we cannot return specialized errors because of the waPC contract
        // hence we have to unfortunately look at the error string
        assert!(err.to_string().as_str().contains("unexpected request"));
    }

    #[test]
    fn record_response_replay_successful_response() {
        let expected_payload = "hello world".to_string();
        let exchanges = vec![parsed_exchange(
            busybox_manifest_digest_request(),
            Response::Success {
                payload: expected_payload.clone(),
            },
        )];
        let cursor = ReplayCursor::default();

        let request = dummy_request(busybox_manifest_digest_request());

        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor)
                .expect("should not be an error");
        assert_eq!(response.payload, expected_payload.into_bytes());
    }

    #[test]
    fn record_response_replay_errored_response() {
        let expected_err_msg = "something went wrong".to_string();
        let exchanges = vec![parsed_exchange(
            busybox_manifest_digest_request(),
            Response::Error {
                message: expected_err_msg.clone(),
            },
        )];
        let cursor = ReplayCursor::default();

        let request = dummy_request(busybox_manifest_digest_request());

        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor);
        assert!(response.is_err());
        let err = response.unwrap_err();
        assert_eq!(err.to_string(), expected_err_msg);
    }

    #[test]
    fn replay_can_be_reset_and_replayed_again() {
        let expected_payload = "hello world".to_string();
        let exchanges = vec![parsed_exchange(
            busybox_manifest_digest_request(),
            Response::Success {
                payload: expected_payload.clone(),
            },
        )];
        let cursor = ReplayCursor::default();

        // First "evaluation": consume the single recorded exchange.
        let request = dummy_request(busybox_manifest_digest_request());
        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor)
                .expect("should not be an error");
        assert_eq!(response.payload, expected_payload.clone().into_bytes());

        // Without a reset, the session is exhausted.
        let request = dummy_request(busybox_manifest_digest_request());
        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor);
        assert!(response.is_err());
        assert!(response.unwrap_err().to_string().contains("empty"));

        // Resetting the cursor allows the same session to be replayed again,
        // as `kwctl bench` does before every benchmark iteration.
        cursor.reset();
        let request = dummy_request(busybox_manifest_digest_request());
        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor)
                .expect("should not be an error after reset");
        assert_eq!(response.payload, expected_payload.into_bytes());
    }

    #[test]
    fn replay_reset_handle_resets_shared_cursor() {
        let exchanges = vec![parsed_exchange(
            busybox_manifest_digest_request(),
            Response::Success {
                payload: "hello world".to_string(),
            },
        )];
        let cursor = Arc::new(ReplayCursor::default());
        let handle = ReplayResetHandle(cursor.clone());

        let request = dummy_request(busybox_manifest_digest_request());
        CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor)
            .expect("should not be an error");
        assert_eq!(cursor.peek(), 1);

        handle.reset();
        assert_eq!(cursor.peek(), 0);

        // The session can be replayed again after resetting through the handle.
        let request = dummy_request(busybox_manifest_digest_request());
        let response =
            CallbackHandlerProxy::produce_recorded_response(&request, &exchanges, &cursor);
        assert!(response.is_ok());
    }
}
