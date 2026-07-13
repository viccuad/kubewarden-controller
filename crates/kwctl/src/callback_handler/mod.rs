use std::path::PathBuf;

use anyhow::Result;
use policy_evaluator::{callback_requests::CallbackRequest, kube};
use tokio::sync::{mpsc, oneshot};

mod proxy;

pub(crate) use proxy::ReplayResetHandle;

use crate::{
    callback_handler::proxy::CallbackHandlerProxy,
    config::{HostCapabilitiesMode, pull_and_run::PullAndRunSettings},
};

#[derive(Clone)]
pub(crate) enum ProxyMode {
    Record { destination: PathBuf },
    Replay { source: PathBuf },
}

/// This is an abstraction over the callback_handler provided by the
/// policy_evaluator crate.
/// The goal is to allow kwctl to have a proxy handler, that can
/// record and reply any kind of policy <-> host capability exchange
pub(crate) enum CallbackHandler {
    Direct(policy_evaluator::callback_handler::CallbackHandler),
    Proxy(proxy::CallbackHandlerProxy),
}

impl CallbackHandler {
    pub async fn new(
        cfg: &PullAndRunSettings,
        kube_client: Option<kube::Client>,
        shutdown_channel_rx: oneshot::Receiver<()>,
    ) -> Result<CallbackHandler> {
        match &cfg.host_capabilities_mode {
            HostCapabilitiesMode::Proxy(proxy_mode) => {
                new_proxy(proxy_mode, cfg, kube_client, shutdown_channel_rx).await
            }
            HostCapabilitiesMode::Direct => {
                new_transparent(cfg, kube_client, shutdown_channel_rx).await
            }
        }
    }

    pub fn sender_channel(&self) -> mpsc::Sender<CallbackRequest> {
        match self {
            CallbackHandler::Direct(handler) => handler.sender_channel(),
            CallbackHandler::Proxy(handler) => handler.sender_channel(),
        }
    }

    /// Returns a handle that can be used to reset a replay session back to
    /// its first recorded exchange. Only meaningful (returns `Some`) when
    /// the callback handler is a proxy running in replay mode; `None`
    /// otherwise (direct mode, or proxy in record mode).
    ///
    /// This is used by `kwctl bench`, which reuses a single evaluator (and
    /// therefore a single replay session) across many evaluations: resetting
    /// the cursor before each evaluation lets a session recorded from one
    /// evaluation be replayed again for every benchmark iteration.
    pub fn replay_reset_handle(&self) -> Option<ReplayResetHandle> {
        match self {
            CallbackHandler::Direct(_) => None,
            CallbackHandler::Proxy(handler) => handler.replay_reset_handle(),
        }
    }

    pub async fn loop_eval(self) {
        match self {
            CallbackHandler::Direct(mut handler) => handler.loop_eval().await,
            CallbackHandler::Proxy(mut handler) => handler.loop_eval().await,
        }
    }
}

async fn new_proxy(
    mode: &ProxyMode,
    cfg: &PullAndRunSettings,
    kube_client: Option<kube::Client>,
    shutdown_channel_rx: oneshot::Receiver<()>,
) -> Result<CallbackHandler> {
    let proxy = CallbackHandlerProxy::new(
        mode,
        shutdown_channel_rx,
        cfg.sources.clone(),
        cfg.sigstore_trust_root.clone(),
        kube_client,
    )
    .await?;

    Ok(CallbackHandler::Proxy(proxy))
}

async fn new_transparent(
    cfg: &PullAndRunSettings,
    kube_client: Option<kube::Client>,
    shutdown_channel_rx: oneshot::Receiver<()>,
) -> Result<CallbackHandler> {
    let mut callback_handler_builder =
        policy_evaluator::callback_handler::CallbackHandlerBuilder::new(shutdown_channel_rx)
            .registry_config(cfg.sources.clone())
            .trust_root(cfg.sigstore_trust_root.clone());
    if let Some(kc) = kube_client {
        callback_handler_builder = callback_handler_builder.kube_client(kc);
    }

    let real_callback_handler = callback_handler_builder.build().await?;

    Ok(CallbackHandler::Direct(real_callback_handler))
}
