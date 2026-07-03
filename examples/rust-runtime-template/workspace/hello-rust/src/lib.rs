pub fn greeting(name: &str) -> String {
    let name = if name.is_empty() { "cube" } else { name };
    format!("hello {name} from Rust inside CubeSandbox")
}

#[cfg(test)]
mod tests {
    use super::greeting;

    #[test]
    fn greeting_uses_default_name() {
        assert_eq!(greeting(""), "hello cube from Rust inside CubeSandbox");
    }

    #[test]
    fn greeting_uses_provided_name() {
        assert_eq!(
            greeting("Alice"),
            "hello Alice from Rust inside CubeSandbox"
        );
    }
}
