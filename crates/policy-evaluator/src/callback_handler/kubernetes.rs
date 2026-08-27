use std::collections::BTreeSet;

mod client;
pub(crate) mod field_mask;
mod reflector;

use anyhow::{Result, anyhow};
pub(crate) use client::Client;
use k8s_openapi::api::authorization::v1::SubjectAccessReviewStatus;
use kube::core::ObjectList;
use kubewarden_policy_sdk::host_capabilities::kubernetes::SubjectAccessReview as KWSubjectAccessReview;
use serde::Serialize;

use crate::callback_handler::cache_return::Return;

#[derive(Eq, Hash, PartialEq)]
struct ApiVersionKind {
    api_version: String,
    kind: String,
}

#[derive(Debug, Clone, Serialize)]
pub(crate) struct KubeResource {
    pub resource: kube::api::ApiResource,
    pub namespaced: bool,
}

pub(crate) async fn list_resources_by_namespace(
    client: Option<&Client>,
    api_version: &str,
    kind: &str,
    namespace: &str,
    label_selector: Option<String>,
    field_selector: Option<String>,
    field_masks: Option<BTreeSet<String>>,
) -> Result<Return<ObjectList<kube::core::DynamicObject>>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly")).map(Return::not_cached);
    }

    client
        .unwrap()
        .list_resources_by_namespace(
            api_version,
            kind,
            namespace,
            label_selector,
            field_selector,
            field_masks,
        )
        .await
        .map(Return::not_cached)
}

pub(crate) async fn list_resources_all(
    client: Option<&Client>,
    api_version: &str,
    kind: &str,
    label_selector: Option<String>,
    field_selector: Option<String>,
    field_masks: Option<BTreeSet<String>>,
) -> Result<Return<ObjectList<kube::core::DynamicObject>>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly")).map(Return::not_cached);
    }

    client
        .unwrap()
        .list_resources_all(
            api_version,
            kind,
            label_selector,
            field_selector,
            field_masks,
        )
        .await
        .map(Return::not_cached)
}

pub(crate) async fn get_resource(
    client: Option<&Client>,
    api_version: &str,
    kind: &str,
    name: &str,
    namespace: Option<&str>,
) -> Result<Return<kube::core::DynamicObject>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly"));
    }

    client
        .unwrap()
        .get_resource(api_version, kind, name, namespace)
        .await
        .map(Return::not_cached)
}

pub(crate) async fn get_resource_cached(
    client: Option<&Client>,
    api_version: &str,
    kind: &str,
    name: &str,
    namespace: Option<&str>,
) -> Result<Return<kube::core::DynamicObject>> {
    match client {
        Some(client) => {
            client
                .get_resource_cached(api_version, kind, name, namespace)
                .await
        }
        None => Err(anyhow!("kube::Client was not initialized properly")),
    }
}

pub(crate) async fn get_resource_plural_name(
    client: Option<&Client>,
    api_version: &str,
    kind: &str,
) -> Result<Return<String>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly"));
    }

    client
        .unwrap()
        .get_resource_plural_name(api_version, kind)
        .await
        // this is always cached, because the client builds an overview of
        // the cluster resources at bootstrap time
        .map(Return::cached)
}

/// Check if the results of the "list all resources" query have changed since the provided instant
/// This is done by querying the reflector that keeps track of this query
pub(crate) async fn has_list_resources_all_result_changed_since_instant(
    client: Option<&Client>,
    api_version: &str,
    kind: &str,
    label_selector: Option<String>,
    field_selector: Option<String>,
    field_masks: Option<BTreeSet<String>>,
    since: tokio::time::Instant,
) -> Result<Return<bool>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly")).map(Return::not_cached);
    }

    client
        .unwrap()
        .has_list_resources_all_result_changed_since_instant(
            api_version,
            kind,
            label_selector,
            field_selector,
            field_masks,
            since,
        )
        .await
        .map(Return::not_cached)
}

pub(crate) async fn can_i(
    client: Option<&Client>,
    request: KWSubjectAccessReview,
) -> Result<Return<SubjectAccessReviewStatus>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly"));
    }

    client.unwrap().can_i(request).await.map(Return::not_cached)
}

pub(crate) async fn can_i_cached(
    client: Option<&Client>,
    request: KWSubjectAccessReview,
) -> Result<Return<SubjectAccessReviewStatus>> {
    match client {
        Some(client) => client.can_i_cached(request).await,
        None => Err(anyhow!("kube::Client was not initialized properly")),
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    use hyper::{Request, Response};
    use k8s_openapi::api::authorization::v1::SubjectAccessReview;
    use kube::client::Body;

    use super::*;

    // Ensure that the can_i cache deduplicates the backend calls. The test calls can_i_cached two
    // times with the same request. The mock API server must receive exactly one SubjectAccessReview
    // POST: the second call is a cache hit.
    #[tokio::test(flavor = "multi_thread")]
    async fn can_i_cached_deduplicates_backend_calls() {
        let (mocksvc, mut handle) = tower_test::mock::pair::<Request<Body>, Response<Body>>();
        let sar_counter = Arc::new(AtomicUsize::new(0));

        let counter = sar_counter.clone();
        tokio::spawn(async move {
            loop {
                let (request, send) = handle.next_request().await.expect("service not called");
                assert_eq!(
                    request.uri().path(),
                    "/apis/authorization.k8s.io/v1/subjectaccessreviews"
                );
                counter.fetch_add(1, Ordering::SeqCst);

                let response = SubjectAccessReview {
                    status: Some(SubjectAccessReviewStatus {
                        allowed: false,
                        ..Default::default()
                    }),
                    ..Default::default()
                };
                let body = serde_json::to_vec(&response).unwrap();
                send.send_response(Response::builder().body(Body::from(body)).unwrap());
            }
        });

        let client = Client::new(kube::Client::new(mocksvc, "default"));
        let request = KWSubjectAccessReview {
            user: "system:serviceaccount:default:can-i-cached-dedup-test".to_owned(),
            ..Default::default()
        };

        let first = can_i_cached(Some(&client), request.clone())
            .await
            .expect("first call failed");
        let second = can_i_cached(Some(&client), request)
            .await
            .expect("second call failed");

        assert!(!first.was_cached);
        assert!(second.was_cached);
        assert_eq!(1, sar_counter.load(Ordering::SeqCst));
    }
}
