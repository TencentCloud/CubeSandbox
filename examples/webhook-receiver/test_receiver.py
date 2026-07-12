import hashlib
import hmac
import os
import unittest

os.environ["CUBE_WEBHOOK_SECRET"] = "test-secret"
import receiver


class SignatureTests(unittest.TestCase):
    def test_valid_signature(self):
        body = b'{"event":"sandbox.created"}'
        signature = "sha256=" + hmac.new(b"test-secret", body, hashlib.sha256).hexdigest()
        self.assertTrue(receiver.valid_signature(body, signature))

    def test_tampered_signature(self):
        self.assertFalse(receiver.valid_signature(b"payload", "sha256=bad"))


if __name__ == "__main__":
    unittest.main()
