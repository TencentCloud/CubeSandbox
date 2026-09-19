use std::collections::HashMap;
use std::sync::{Mutex, OnceLock};

/// Process-wide defaults delivered by `POST /init`. New processes and PTYs
/// merge these in, with per-request values taking precedence.
#[derive(Clone, Debug, Default)]
pub(crate) struct Defaults {
    pub(crate) env_vars: HashMap<String, String>,
    pub(crate) user: Option<String>,
    pub(crate) workdir: Option<String>,
    pub(crate) access_token: Option<String>,
}

/// Values applied by `POST /init`.
#[derive(Debug, Default)]
pub(crate) struct InitDefaults {
    pub(crate) env_vars: HashMap<String, String>,
    pub(crate) user: Option<String>,
    pub(crate) workdir: Option<String>,
    pub(crate) access_token: Option<String>,
}

fn store() -> &'static Mutex<Defaults> {
    static STORE: OnceLock<Mutex<Defaults>> = OnceLock::new();
    STORE.get_or_init(|| Mutex::new(Defaults::default()))
}

pub(crate) fn snapshot() -> Defaults {
    store().lock().expect("defaults lock poisoned").clone()
}

pub(crate) fn update(update: InitDefaults) {
    let mut defaults = store().lock().expect("defaults lock poisoned");
    defaults.env_vars.extend(update.env_vars);
    if let Some(user) = update.user.filter(|user| !user.is_empty()) {
        defaults.user = Some(user);
    }
    if let Some(workdir) = update.workdir.filter(|workdir| !workdir.is_empty()) {
        defaults.workdir = Some(workdir);
    }
    if let Some(token) = update.access_token.filter(|token| !token.is_empty()) {
        defaults.access_token = Some(token);
    }
}

impl Defaults {
    pub(crate) fn user_or_root(&self) -> String {
        self.user
            .as_deref()
            .filter(|user| !user.is_empty())
            .unwrap_or("root")
            .to_owned()
    }

    pub(crate) fn workdir(&self) -> Option<String> {
        self.workdir
            .as_deref()
            .filter(|workdir| !workdir.is_empty())
            .map(str::to_owned)
    }

    /// Defaults first, then request env vars so the caller can override.
    pub(crate) fn merged_env_vars(
        &self,
        request: &HashMap<String, String>,
    ) -> HashMap<String, String> {
        let mut env_vars = self.env_vars.clone();
        env_vars.extend(
            request
                .iter()
                .map(|(key, value)| (key.clone(), value.clone())),
        );
        env_vars
    }

    pub(crate) fn access_token(&self) -> Option<String> {
        self.access_token
            .as_deref()
            .filter(|token| !token.is_empty())
            .map(str::to_owned)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_env_vars_override_defaults() {
        let defaults = Defaults {
            env_vars: HashMap::from([
                ("FOO".to_owned(), "default".to_owned()),
                ("KEEP".to_owned(), "default".to_owned()),
            ]),
            ..Defaults::default()
        };
        let request = HashMap::from([("FOO".to_owned(), "request".to_owned())]);
        let merged = defaults.merged_env_vars(&request);

        assert_eq!(merged.get("FOO").map(String::as_str), Some("request"));
        assert_eq!(merged.get("KEEP").map(String::as_str), Some("default"));
    }

    #[test]
    fn empty_user_and_workdir_fall_back() {
        let defaults = Defaults {
            user: Some(String::new()),
            workdir: Some(String::new()),
            ..Defaults::default()
        };
        assert_eq!(defaults.user_or_root(), "root");
        assert_eq!(defaults.workdir(), None);
        assert_eq!(defaults.access_token(), None);
    }
}
