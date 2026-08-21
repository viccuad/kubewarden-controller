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
