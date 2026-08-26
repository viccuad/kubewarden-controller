use std::{future::Future, hash::Hash};

use anyhow::{Result, anyhow};

/// Wraps a callback handler function result. The `was_cached` flag shows if the value came from the
/// cache.
#[derive(Debug, Clone)]
pub(crate) struct Return<T> {
    pub value: T,
    pub was_cached: bool,
}

impl<T> Return<T> {
    /// Wraps a value that did not come from the cache.
    pub fn new(value: T) -> Self {
        Return {
            value,
            was_cached: false,
        }
    }
}

/// Looks up `key` inside of `cache`. On a miss, `init` is awaited to compute the value, which is
/// then stored inside of the cache. Only successful results enter the cache.
///
/// This centralizes the moka `entry().or_try_insert_with()` dance used by all the caches owned by
/// the callback handler's clients (Kubernetes, OCI registry, Sigstore verification).
pub(crate) async fn try_cached<K, V>(
    cache: &moka::future::Cache<K, V>,
    key: K,
    init: impl Future<Output = Result<V>>,
) -> Result<Return<V>>
where
    K: Hash + Eq + Send + Sync + Clone + 'static,
    V: Clone + Send + Sync + 'static,
{
    let entry = cache
        .entry(key)
        .or_try_insert_with(init)
        .await
        .map_err(|e| anyhow!("{e:#}"))?;

    Ok(Return {
        was_cached: !entry.is_fresh(),
        value: entry.into_value(),
    })
}

impl<T> std::ops::Deref for Return<T> {
    type Target = T;

    fn deref(&self) -> &T {
        &self.value
    }
}

impl<T> std::ops::DerefMut for Return<T> {
    fn deref_mut(&mut self) -> &mut T {
        &mut self.value
    }
}
