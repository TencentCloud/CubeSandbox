import copy
import unittest

from conformance import norm_stream_frames, normalize


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
