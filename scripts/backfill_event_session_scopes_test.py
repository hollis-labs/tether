import importlib.util
from pathlib import Path
import sqlite3
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("backfill", Path(__file__).with_name("backfill-event-session-scopes.py"))
backfill = importlib.util.module_from_spec(spec)
spec.loader.exec_module(backfill)


class BackfillCopyTest(unittest.TestCase):
    def test_copy_only_wal_preservation_isolation_and_idempotence(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "source.db"
            output = Path(directory) / "copy.db"
            with sqlite3.connect(source) as db:
                db.execute("PRAGMA journal_mode=WAL")
                db.executescript("""
                    CREATE TABLE sessions(id TEXT PRIMARY KEY, task_id TEXT, workstream_id TEXT,
                                          provider_id TEXT, session_group_id TEXT);
                    CREATE TABLE team_runs(run_id TEXT, session_group_id TEXT UNIQUE);
                    CREATE TABLE session_provider_mappings(session_id TEXT, owner TEXT,
                                                          provider TEXT, native_session_id TEXT);
                    CREATE TABLE events(id INTEGER PRIMARY KEY,session_id TEXT, process_provider TEXT,
                      scope_task_id TEXT,scope_workstream_id TEXT,scope_run_id TEXT,provider_session_id TEXT,
                      payload_json TEXT, event_id TEXT, pending INTEGER);
                    INSERT INTO sessions VALUES('s','task','stream','provider','group');
                    INSERT INTO sessions VALUES('empty',NULL,NULL,'provider',NULL);
                    INSERT INTO team_runs VALUES('run','group');
                    INSERT INTO session_provider_mappings VALUES('s','foreign','provider','wrong');
                    INSERT INTO session_provider_mappings VALUES('s','tether','provider','native');
                    INSERT INTO session_provider_mappings VALUES('s','tether','other','other-native');
                    INSERT INTO events VALUES(1,'s',NULL,NULL,NULL,NULL,NULL,'untouched','id-1',1);
                    INSERT INTO events VALUES(2,'s','other','explicit-task','explicit-stream','explicit-run','explicit-native','untouched','id-2',0);
                    INSERT INTO events VALUES(4,'unknown',NULL,NULL,NULL,NULL,NULL,'untouched','id-4',0);
                    INSERT INTO events VALUES(5,NULL,NULL,NULL,NULL,NULL,NULL,'untouched','id-5',0);
                    INSERT INTO events VALUES(6,'empty',NULL,NULL,NULL,NULL,NULL,'untouched','id-6',0);
                    INSERT INTO events VALUES(7,'s','missing','','','','','untouched','id-7',0);
                    INSERT INTO events VALUES(8,'s','other',NULL,NULL,NULL,NULL,'untouched','id-8',0);
                """)
                # Keep connection open so committed events are still in the WAL.
                original = db.execute("SELECT * FROM events ORDER BY id").fetchall()
                report = backfill.backfill_copy(source, output, batch_size=2)
                self.assertEqual(db.execute("SELECT * FROM events ORDER BY id").fetchall(), original)
                self.assertEqual(report["changed_rows"], 3)
                self.assertEqual(report["after"]["filled"]["provider_session_id"], 3)
            with sqlite3.connect(output) as db:
                rows = db.execute("SELECT * FROM events ORDER BY id").fetchall()
                self.assertEqual(rows[0][3:7], ('task','stream','run','native'))
                self.assertEqual(rows[1], original[1])
                self.assertEqual(rows[2:5], original[2:5])
                self.assertIsNone(rows[5][6])
                self.assertEqual(rows[6][6], 'other-native')
                for before, after in zip(original, rows):
                    self.assertEqual(before[7:], after[7:])
            second = backfill.backfill_copy(output, Path(directory) / "second.db", 1)
            self.assertEqual(second["changed_rows"], 0)
            self.assertEqual(second["before"], second["after"])
            with self.assertRaises(FileExistsError):
                backfill.backfill_copy(source, output)
            with self.assertRaises(FileExistsError):
                backfill.backfill_copy(source, source)
            with self.assertRaises(ValueError):
                backfill.backfill_copy(source, Path(directory) / "invalid.db", 0)


if __name__ == '__main__':
    unittest.main()
