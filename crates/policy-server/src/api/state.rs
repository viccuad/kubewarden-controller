use std::sync::Arc;

use tokio::sync::Semaphore;

use crate::evaluation::EvaluationEnvironment;

pub(crate) struct ApiServerState {
    pub(crate) semaphore: Semaphore,
    pub(crate) evaluation_environment: Arc<EvaluationEnvironment>,
}
