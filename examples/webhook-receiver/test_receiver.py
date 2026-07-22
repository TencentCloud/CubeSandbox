# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import hmac
import unittest

from receiver import expected_signature


class SignatureTest(unittest.TestCase):
    def test_expected_signature_matches_hmac_sha256_contract(self) -> None:
        body = b'{"event":"sandbox.created","sandbox_id":"sbx-1"}'
        timestamp = "1784678400"
        nonce = "nonce-1"
        signature = expected_signature("secret", timestamp, nonce, body)

        self.assertTrue(signature.startswith("sha256="))
        self.assertTrue(
            hmac.compare_digest(
                signature,
                "sha256=4d517163105f8aea5c37b35dd132611ac1d2d58e5af1d81ec34d529bb14ad3cd",
            )
        )


if __name__ == "__main__":
    unittest.main()
