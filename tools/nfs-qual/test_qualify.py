import copy
import tempfile
import unittest
from pathlib import Path

from qualify import attribute_trace, confirmed_phase_coverage


class QualificationEvidenceTests(unittest.TestCase):
    def test_many_small_foreign_writes_cannot_hide_in_rpc_overhead(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "trace"
            path.write_text(
                "x nfs_initiate_write: fileid=00:31:101 count=100000\n"
                + "x nfs_initiate_write: fileid=00:31:202 count=4096\n"*200
                + "x nfs_initiate_read: fileid=00:32:303 count=100000\n")
            record = self.record()
            result = attribute_trace(path, record)
            self.assertTrue(result["valid"], result)
            self.assertEqual(result["ownWriteBytes"], 100000)
            self.assertEqual(result["foreignWriteBytes"], 200*4096)
            self.assertGreater(result["foreignWriteBytes"] / record["size"], 0.05)
            record["traceDone"]["perCPU"] = {"cpu0": "overrun: 1\ndropped events: 0\n"}
            self.assertFalse(attribute_trace(path, record)["valid"])

    def test_late_or_failed_signal_is_not_phase_coverage(self):
        record = {"stop": {"signal": "TERM", "phase": "final-sync"},
                  "signalSent": True, "waitCode": 2, "state": {"OOMKilled": False},
                  "events": [{"phase": "signal-received", "signal_number": 15,
                              "observed_phase": "final-sync"},
                             {"phase": "finished", "result": "cancelled"}]}
        self.assertTrue(confirmed_phase_coverage(record))
        for change in ("late", "storage_failure", "KILL", "OOM", "wrong_signal"):
            altered = copy.deepcopy(record)
            if change == "late":
                altered["events"][0]["observed_phase"] = "close-returned"
            elif change == "storage_failure":
                altered["events"][1]["result"] = "failed"
            elif change == "KILL":
                altered["stop"]["signal"] = "KILL"
            elif change == "OOM":
                altered["state"]["OOMKilled"] = True
            else:
                altered["events"][0]["signal_number"] = 2
            self.assertFalse(confirmed_phase_coverage(altered), change)

    @staticmethod
    def record():
        return {"size": 100000, "completedPolicyWork": True, "exitVerifiedAt": 20,
                "events": [{"phase": "target-created", "device": 49, "inode": 101},
                           {"phase": "source-opened", "device": 50, "inode": 303}],
                "traceReady": {"ino64": "Y", "device": 49, "sourceDevice": 50},
                "traceDone": {"stopReason": "controller", "endWall": 21,
                              "perCPU": {"cpu0": "overrun: 0\ndropped events: 0\n"}}}


if __name__ == "__main__":
    unittest.main()
