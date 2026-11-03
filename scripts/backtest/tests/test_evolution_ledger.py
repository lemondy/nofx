import json
import tempfile
import unittest
from pathlib import Path

from scripts.backtest import evolution_ledger as ledger


def row(stage='S1', verdict='fail', **extra):
    base = dict(baseline_version='h0', change_type='B', stage=stage, idea_id='i', verdict=verdict)
    return {**base, **extra}


class EvolutionLedgerTest(unittest.TestCase):
    def test_k_counts_non_invalid_s1_per_track(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'ledger.jsonl'
            a = ledger.append_event(path, 'crypto_llm', lambda k, _: row())
            b = ledger.append_event(path, 'crypto_llm', lambda k, _: row(verdict='invalid'))
            c = ledger.append_event(path, 'crypto_llm', lambda k, _: row(verdict='screen_pass'))
            d = ledger.append_event(path, 'us_llm', lambda k, _: row())
            e = ledger.append_event(path, 'crypto_llm', lambda k, _: row(stage='S2', verdict='promote'))
            self.assertEqual([a['K'], b['K'], c['K'], d['K'], e['K']], [1, None, 2, 1, None])
            self.assertEqual(ledger.next_k(ledger.read_rows(path), 'crypto_llm'), 3)

    def test_build_sees_k_before_verdict(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'ledger.jsonl'
            ledger.append_event(path, 'fq_crypto', lambda k, _: row())
            seen = []
            ledger.append_event(path, 'fq_crypto', lambda k, prev: seen.append((k, len(prev))) or row())
            self.assertEqual(seen, [(2, 1)])

    def test_append_only_and_validation(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'ledger.jsonl'
            ledger.append_event(path, 'crypto_llm', lambda k, _: row(delta=float('inf')))
            first = path.read_bytes()
            with self.assertRaises(ValueError):
                ledger.append_event(path, 'crypto_llm', lambda k, _: row(change_type='Z'))
            with self.assertRaises(ValueError):
                ledger.append_event(path, 'nope', lambda k, _: row())
            self.assertEqual(path.read_bytes(), first)
            self.assertEqual(json.loads(first)['delta'], 'inf')
            path.write_bytes(first + b'{"partial"')
            with self.assertRaises(ValueError):
                ledger.append_event(path, 'crypto_llm', lambda k, _: row())


if __name__ == '__main__':
    unittest.main()
