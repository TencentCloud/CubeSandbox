import hashlib
import hmac
import os
import unittest

os.environ["CUBE_WEBHOOK_SECRET"] = "test-secret"
import receiver


class SignatureTests(unittest.TestCase):
    def test_valid_signature(self):
        body = b'{"event":"sandbox.created"}'
        timestamp = "1700000000"
        signature = "sha256=" + hmac.new(
            b"test-secret", timestamp.encode() + b"." + body, hashlib.sha256
        ).hexdigest()
        self.assertTrue(receiver.valid_signature(body, signature, timestamp, 1700000000))

    def test_tampered_signature(self):
        self.assertFalse(
            receiver.valid_signature(b"payload", "sha256=bad", "1700000000", 1700000000)
        )

    def test_stale_signature(self):
        self.assertFalse(
            receiver.valid_signature(b"payload", "sha256=bad", "1699999000", 1700000000)
        )


if __name__ == "__main__":
    unittest.main()
