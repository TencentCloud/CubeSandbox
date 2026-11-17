import copy
import json
import unittest

from conformance import norm_stream_frames, normalize, normalize_fixture


class SignalErrorNormalizationTests(unittest.TestCase):
    def response(self, message):
        return {"status": 400, "headers": {"Content-Type": "application/json"},
                "body": json.dumps({"code": "invalid_argument", "message": message})}

    def test_only_decoder_context_is_normalized(self):
        rust = self.response("unmarshal message: invalid value for enum field signal")
        for selector in ("", "missing_", "empty_"):
            for kind, token in (("bool", "true"), ("object", "{"), ("array", "["),
                                ("float", "1.5"), ("out_of_range", "2147483648")):
                name = f"proc_send_signal_{selector}{kind}"
                for position in ("1:36", "3:8"):
                    go = self.response(
                        "unmarshal message: unmarshal into *process.SendSignalRequest: "
                        f"proto: (line {position}): invalid value for enum field signal: {token}")
                    self.assertEqual(normalize_fixture(name, go), normalize_fixture(name, rust))

    def test_status_code_reason_token_and_other_fields_remain_checked(self):
        name = "proc_send_signal_bool"
        message = ("unmarshal message: unmarshal into *process.SendSignalRequest: "
                   "proto: (line 1:36): invalid value for enum field signal: true")
        go = self.response(message)
        expected = normalize_fixture(name, go)
        for status in (200, 404, 501):
            actual = copy.deepcopy(go)
            actual["status"] = status
            self.assertNotEqual(expected, normalize_fixture(name, actual))
        for key, value in (("code", "not_found"), ("extra", "unexpected"),
                           ("message", message.replace("signal:", "pid:")),
                           ("message", message.replace("true", "false")),
                           ("message", message + " unexpected")):
            actual = copy.deepcopy(go)
            body = json.loads(actual["body"])
            body[key] = value
            actual["body"] = json.dumps(body)
            self.assertNotEqual(expected, normalize_fixture(name, actual))
        actual = copy.deepcopy(go)
        actual["headers"]["Content-Type"] = "text/plain"
        self.assertNotEqual(expected, normalize_fixture(name, actual))
        self.assertNotEqual(expected, normalize_fixture("unrelated_fixture", go))


class ExtensionNormalizationTests(unittest.TestCase):
    def test_summary_envelope_length_does_not_depend_on_pid_digits(self):
        baseline = {"first_frame": {"flags": 0, "size": 30,
                    "payload": {"event": {"start": {"pid": 123}}}}}
        actual = {"first_frame": {"flags": 0, "size": 31,
                  "payload": {"event": {"start": {"pid": 1234}}}}}
        self.assertEqual(normalize(norm_stream_frames(baseline)), normalize(norm_stream_frames(actual)))
        self.assertNotEqual(norm_stream_frames({"entry": {"size": 30}}),
                            norm_stream_frames({"entry": {"size": 31}}))

    def fixture(self, end):
        return {"stream": {"status_line": "HTTP/1.1 200 OK", "frames": [
            {"flags": 0, "size": 123, "payload": {"event": {"end": end}}},
            {"flags": 2, "payload": {}},
        ]}}

    def test_nested_signal_metadata_is_the_only_ignored_difference(self):
        end = {"exitCode": -1, "status": "signal: killed", "error": "signal: killed"}
        baseline = self.fixture(end)
        extended = self.fixture({**end, "signal": 9, "killedBy": "user", "oomKilled": False})
        extended["stream"]["frames"][0]["size"] = 456
        self.assertEqual(normalize(norm_stream_frames(baseline)), normalize(norm_stream_frames(extended)))
        extended["stream"]["frames"][0]["payload"]["event"]["end"]["exitCode"] = 0
        self.assertNotEqual(normalize(norm_stream_frames(baseline)), normalize(norm_stream_frames(extended)))

    def test_timeout_extension_requires_exact_end_and_deadline_trailer(self):
        deadline = {"flags": 2, "payload": {"error": {"code": "deadline_exceeded"}}}
        extension = {"flags": 0, "payload": {"event": {"end": {
            "exitCode": -1, "status": "signal: killed", "error": "signal: killed",
            "signal": 9, "killedBy": "timeout",
        }}}}
        baseline = {"frames": [deadline]}
        actual = {"frames": [extension, deadline]}
        self.assertEqual(norm_stream_frames(baseline), norm_stream_frames(actual))
        unexpected = copy.deepcopy(actual)
        unexpected["frames"][0]["payload"]["event"]["end"]["killedBy"] = "user"
        self.assertNotEqual(norm_stream_frames(baseline), norm_stream_frames(unexpected))
        unexpected = copy.deepcopy(actual)
        unexpected["frames"][1]["payload"]["error"]["code"] = "internal"
        self.assertNotEqual(norm_stream_frames(baseline), norm_stream_frames(unexpected))


if __name__ == "__main__":
    unittest.main()
