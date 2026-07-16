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
        nonce = "test-nonce"
        signature = "sha256=" + hmac.new(
            b"test-secret",
            timestamp.encode() + b"." + nonce.encode() + b"." + body,
            hashlib.sha256,
        ).hexdigest()
        self.assertTrue(
            receiver.valid_signature(body, signature, timestamp, nonce, 1700000000)
        )

    def test_tampered_signature(self):
        self.assertFalse(
            receiver.valid_signature(
                b"payload", "sha256=bad", "1700000000", "nonce", 1700000000
            )
        )

    def test_stale_signature(self):
        self.assertFalse(
            receiver.valid_signature(
                b"payload", "sha256=bad", "1699999000", "nonce", 1700000000
            )
        )

    def test_replayed_nonce_is_rejected(self):
        receiver._seen_nonces.clear()
        self.assertTrue(receiver.claim_nonce("nonce-1", 1700000000))
        self.assertFalse(receiver.claim_nonce("nonce-1", 1700000001))
        self.assertTrue(receiver.claim_nonce("nonce-1", 1700000301))


if __name__ == "__main__":
    unittest.main()
