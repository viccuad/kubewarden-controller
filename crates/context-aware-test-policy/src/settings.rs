use serde::{Deserialize, Serialize};
use slog::info;

use crate::LOG_DRAIN;

// Describe the settings your policy expects when
// loaded by the policy server.
#[derive(Serialize, Deserialize, Default, Debug)]
#[serde(default)]
pub(crate) struct Settings {}

impl kubewarden::settings::Validatable for Settings {
    fn validate(&self) -> Result<(), String> {
        info!(LOG_DRAIN, "starting settings validation");

        // TODO: perform settings validation if applies
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use kubewarden_policy_sdk::settings::Validatable;

    use super::*;

    #[test]
    fn validate_settings() -> Result<(), ()> {
        let settings = Settings {};

        assert!(settings.validate().is_ok());
        Ok(())
    }
}
