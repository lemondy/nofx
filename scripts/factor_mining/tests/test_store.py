import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np

from scripts.factor_mining import miner, store


class AppendStoreTests(unittest.TestCase):
    def test_append_preserves_inode_and_prefix_and_fsyncs(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto' / 'ledger.jsonl'
            store.append_jsonl(path, [{'n': 1}])
            prefix, inode = path.read_bytes(), path.stat().st_ino
            with patch.object(store, '_replace', side_effect=AssertionError('must append')), \
                 patch.object(store.os, 'fsync', wraps=store.os.fsync) as sync:
                store.append_jsonl(path, [{'n': np.int64(2), 't': np.nan}])
            sync.assert_called_once()
            self.assertEqual(path.stat().st_ino, inode)
            self.assertTrue(path.read_bytes().startswith(prefix))
            self.assertEqual(store.read_jsonl(path), [{'n': 1}, {'n': 2, 't': None}])
            self.assertTrue(path.read_bytes().endswith(b'\n'))

    def test_torn_tail_warns_and_next_append_is_readable(self):
        for tail in (b'{"n":', b'{"text":"\xe4\xb8'):
            with self.subTest(tail=tail), tempfile.TemporaryDirectory() as root:
                path = Path(root) / 'ledger.jsonl'
                path.write_bytes(b'{"n":1}\n' + tail)
                with self.assertLogs(store.__name__, level='WARNING') as log:
                    self.assertEqual(store.read_jsonl(path), [{'n': 1}])
                self.assertIn(':2', log.output[0])
                before = path.read_bytes()
                store.append_jsonl(path, [{'n': 3}])
                self.assertTrue(path.read_bytes().startswith(before))
                with self.assertLogs(store.__name__, level='WARNING'):
                    self.assertEqual(store.read_jsonl(path), [{'n': 1}, {'n': 3}])

    def test_complete_unterminated_tail_and_empty_append(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'ledger.jsonl'
            store.append_jsonl(path, [])
            self.assertFalse(path.exists())
            self.assertEqual(store.read_jsonl(path), [])
            path.write_text(json.dumps({'n': 1}))
            store.append_jsonl(path, [{'n': 2}])
            self.assertEqual(store.read_jsonl(path), [{'n': 1}, {'n': 2}])

    def test_only_evaluator_and_expression_are_frozen(self):
        self.assertEqual(set(miner.frozen_hashes()), {'evaluator_sha256', 'expr_sha256'})
