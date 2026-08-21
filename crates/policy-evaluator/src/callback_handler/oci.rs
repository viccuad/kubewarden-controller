use std::sync::LazyLock;
use std::time::Duration;

use anyhow::{Result, anyhow};

use crate::callback_handler::cache_return::Return;
use kubewarden_policy_sdk::host_capabilities::oci::ManifestDigestResponse;
use policy_fetcher::{
    oci_client::{
        Reference,
        manifest::{OciImageManifest, OciManifest},
    },
    registry::Registry,
    sources::Sources,
};
use serde::{Deserialize, Serialize};

/// Helper struct to interact with an OCI registry
pub(crate) struct Client {
    sources: Option<Sources>,
    registry: Registry,
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct ManifestAndConfigResponse {
    pub manifest: OciImageManifest,
    pub digest: String,
    pub config: serde_json::Value,
}

impl Client {
    pub fn new(sources: Option<Sources>) -> Self {
        let registry = Registry {};
        Client { sources, registry }
    }

    /// Fetch the manifest digest of the OCI resource referenced via `image`
    pub async fn digest(&self, image: &str) -> Result<String> {
        // this is needed to expand names as `busybox` into
        // fully resolved references like `docker.io/library/busybox`
        let image_ref: Reference = image.parse()?;

        let image_with_proto = format!("registry://{}", image_ref.whole());
        let image_digest = self
            .registry
            .manifest_digest(&image_with_proto, self.sources.as_ref())
            .await?;

        Ok(image_digest)
    }

    pub async fn manifest(&self, image: &str) -> Result<OciManifest> {
        // this is needed to expand names as `busybox` into
        // fully resolved references like `docker.io/library/busybox`
        let image_ref: Reference = image.parse()?;

        let image_with_proto = format!("registry://{}", image_ref.whole());
        let manifest = self
            .registry
            .manifest(&image_with_proto, self.sources.as_ref())
            .await?;
        Ok(manifest)
    }

    pub async fn manifest_and_config(&self, image: &str) -> Result<ManifestAndConfigResponse> {
        // this is needed to expand names as `busybox` into
        // fully resolved references like `docker.io/library/busybox`
        let image_ref: Reference = image.parse()?;
        let image_with_proto = format!("registry://{}", image_ref.whole());
        let (manifest, digest, config) = self
            .registry
            .manifest_and_config(&image_with_proto, self.sources.as_ref())
            .await?;
        Ok(ManifestAndConfigResponse {
            manifest,
            digest,
            config,
        })
    }
}

// A query to a remote OCI registry is slow. This cache keeps the digest results.
// This cache is time bound. moka removes entries 60 seconds after insertion, thus the memory
// usage follows the current request rate (issue #1950).
// The key is the image reference. The client is not part of the key, because the client is
// always the same.
// Only successful results enter the cache.
static OCI_DIGEST_CACHE: LazyLock<moka::future::Cache<String, ManifestDigestResponse>> =
    LazyLock::new(|| {
        moka::future::Cache::builder()
            .time_to_live(Duration::from_secs(60))
            .build()
    });

pub(crate) async fn get_oci_digest_cached(
    oci_client: &Client,
    img: &str,
) -> Result<Return<ManifestDigestResponse>> {
    let entry = OCI_DIGEST_CACHE
        .entry(img.to_owned())
        .or_try_insert_with(async {
            oci_client
                .digest(img)
                .await
                .map(|digest| ManifestDigestResponse { digest })
        })
        .await
        .map_err(|e| anyhow!("{e:#}"))?;

    Ok(Return {
        was_cached: !entry.is_fresh(),
        value: entry.into_value(),
    })
}

// A query to a remote OCI registry is slow. This cache keeps the manifest results.
// This cache is time bound. moka removes entries 60 seconds after insertion, thus the memory
// usage follows the current request rate (issue #1950).
// The key is the image reference. The client is not part of the key, because the client is
// always the same.
// Only successful results enter the cache.
static OCI_MANIFEST_CACHE: LazyLock<moka::future::Cache<String, OciManifest>> =
    LazyLock::new(|| {
        moka::future::Cache::builder()
            .time_to_live(Duration::from_secs(60))
            .build()
    });

pub(crate) async fn get_oci_manifest_cached(
    oci_client: &Client,
    img: &str,
) -> Result<Return<OciManifest>> {
    let entry = OCI_MANIFEST_CACHE
        .entry(img.to_owned())
        .or_try_insert_with(oci_client.manifest(img))
        .await
        .map_err(|e| anyhow!("{e:#}"))?;

    Ok(Return {
        was_cached: !entry.is_fresh(),
        value: entry.into_value(),
    })
}

// A query to a remote OCI registry is slow. This cache keeps the manifest and configuration
// results.
// This cache is time bound. moka removes entries 60 seconds after insertion, thus the memory
// usage follows the current request rate (issue #1950).
// The key is the image reference. The client is not part of the key, because the client is
// always the same.
// Only successful results enter the cache.
static OCI_MANIFEST_AND_CONFIG_CACHE: LazyLock<
    moka::future::Cache<String, ManifestAndConfigResponse>,
> = LazyLock::new(|| {
    moka::future::Cache::builder()
        .time_to_live(Duration::from_secs(60))
        .build()
});

pub(crate) async fn get_oci_manifest_and_config_cached(
    oci_client: &Client,
    img: &str,
) -> Result<Return<ManifestAndConfigResponse>> {
    let entry = OCI_MANIFEST_AND_CONFIG_CACHE
        .entry(img.to_owned())
        .or_try_insert_with(oci_client.manifest_and_config(img))
        .await
        .map_err(|e| anyhow!("{e:#}"))?;

    Ok(Return {
        was_cached: !entry.is_fresh(),
        value: entry.into_value(),
    })
}
