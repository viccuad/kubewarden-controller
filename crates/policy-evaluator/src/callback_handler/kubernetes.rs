use std::collections::BTreeSet;
use std::sync::LazyLock;
use std::time::Duration;

mod client;
pub(crate) mod field_mask;
mod reflector;

use anyhow::{Result, anyhow};

use crate::callback_handler::cache_return::Return;
use k8s_openapi::api::authorization::v1::SubjectAccessReviewStatus;
use kube::core::ObjectList;
use kubewarden_policy_sdk::host_capabilities::kubernetes::SubjectAccessReview as KWSubjectAccessReview;
use serde::Serialize;

pub(crate) use client::Client;

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
    client: Option<&mut Client>,
    api_version: &str,
    kind: &str,
    namespace: &str,
    label_selector: Option<String>,
    field_selector: Option<String>,
    field_masks: Option<BTreeSet<String>>,
) -> Result<Return<ObjectList<kube::core::DynamicObject>>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly")).map(Return::new);
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
        .map(Return::new)
}

pub(crate) async fn list_resources_all(
    client: Option<&mut Client>,
    api_version: &str,
    kind: &str,
    label_selector: Option<String>,
    field_selector: Option<String>,
    field_masks: Option<BTreeSet<String>>,
) -> Result<Return<ObjectList<kube::core::DynamicObject>>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly")).map(Return::new);
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
        .map(Return::new)
}

pub(crate) async fn get_resource(
    client: Option<&mut Client>,
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
        .map(|value| Return {
            was_cached: false,
            value,
        })
}

// A query to the Kubernetes API server is slow. This cache keeps the resource results,
// which are full Kubernetes objects.
// This cache is time bound. moka removes entries 5 seconds after insertion, thus the memory
// usage follows the current request rate (issue #1950).
// The key is the resource identity: API version, kind, name, and namespace.
// Only successful results enter the cache.
static GET_RESOURCE_CACHE: LazyLock<moka::future::Cache<String, kube::core::DynamicObject>> =
    LazyLock::new(|| {
        moka::future::Cache::builder()
            .time_to_live(Duration::from_secs(5))
            .build()
    });

pub(crate) async fn get_resource_cached(
    client: Option<&mut Client>,
    api_version: &str,
    kind: &str,
    name: &str,
    namespace: Option<&str>,
) -> Result<Return<kube::core::DynamicObject>> {
    let key = format!("get_resource_cached({api_version},{kind}),{name},{namespace:?}");
    let entry = GET_RESOURCE_CACHE
        .entry(key)
        .or_try_insert_with(async {
            get_resource(client, api_version, kind, name, namespace)
                .await
                .map(|response| response.value)
        })
        .await
        .map_err(|e| anyhow!("{e:#}"))?;

    Ok(Return {
        was_cached: !entry.is_fresh(),
        value: entry.into_value(),
    })
}

pub(crate) async fn get_resource_plural_name(
    client: Option<&mut Client>,
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
        .map(|value| Return {
            // this is always cached, because the client builds an overview of
            // the cluster resources at bootstrap time
            was_cached: true,
            value,
        })
}

/// Check if the results of the "list all resources" query have changed since the provided instant
/// This is done by querying the reflector that keeps track of this query
pub(crate) async fn has_list_resources_all_result_changed_since_instant(
    client: Option<&mut Client>,
    api_version: &str,
    kind: &str,
    label_selector: Option<String>,
    field_selector: Option<String>,
    field_masks: Option<BTreeSet<String>>,
    since: tokio::time::Instant,
) -> Result<Return<bool>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly")).map(Return::new);
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
        .map(Return::new)
}

pub(crate) async fn can_i(
    client: Option<&mut Client>,
    request: KWSubjectAccessReview,
) -> Result<Return<SubjectAccessReviewStatus>> {
    if client.is_none() {
        return Err(anyhow!("kube::Client was not initialized properly"));
    }

    client.unwrap().can_i(request).await.map(|value| Return {
        was_cached: false,
        value,
    })
}

// A query to the Kubernetes API server is slow. This cache keeps the SubjectAccessReview
// results.
// This cache is time bound. moka removes entries 5 seconds after insertion, thus the memory
// usage follows the current request rate (issue #1950).
// The key is the full SubjectAccessReview request, because the type implements the Hash and
// Eq traits.
// Only successful results enter the cache.
static CAN_I_CACHE: LazyLock<
    moka::future::Cache<KWSubjectAccessReview, SubjectAccessReviewStatus>,
> = LazyLock::new(|| {
    moka::future::Cache::builder()
        .time_to_live(Duration::from_secs(5))
        .build()
});

pub(crate) async fn can_i_cached(
    client: Option<&mut Client>,
    request: KWSubjectAccessReview,
) -> Result<Return<SubjectAccessReviewStatus>> {
    let entry = CAN_I_CACHE
        .entry(request.clone())
        .or_try_insert_with(async { can_i(client, request).await.map(|response| response.value) })
        .await
        .map_err(|e| anyhow!("{e:#}"))?;

    Ok(Return {
        was_cached: !entry.is_fresh(),
        value: entry.into_value(),
    })
}
