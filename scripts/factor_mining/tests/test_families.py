import random
import unittest

from scripts.factor_mining import zoo
from scripts.factor_mining.families import family_fine
from scripts.factor_mining.miner import family, mutate


class FamilyFineTests(unittest.TestCase):
    def test_keys_follow_operator_window_and_flavor(self):
        cases = {
            'Slope($close,20)/$close': 'trend:m:p',
            'Resi($close,5)': 'trend:s:p',
            'Corr($close,Log($volume+1),5)': 'corr:s:v',
            'Cov($close,$vwap,60)': 'corr:l:p',
            'Std($close,30)/$close': 'disp:m:p',
            'Mean(Std($ret,10),60)': 'disp:l:p',        # longest window decides the band
            'CSRank(Min($low,20)/$close)': 'extreme:m:x',
            'Quantile($close,40,0.2)/$close': 'extreme:l:p',
            'Mean($volume,10)/Mean($volume,60)': 'mean:l:v',
            'EMA($close,7)': 'mean:s:p',
            'IdxMax($high,60)-IdxMin($low,60)': 'misc:l:p',
            'Ref($close,20)/$close': 'misc:m:p',
            '($close-$open)/(($high-$low)+1e-12)': 'misc:s:p',
            'CSRank($close/$open)': 'misc:s:x',
        }
        for expr, key in cases.items():
            with self.subTest(expr=expr):
                self.assertEqual(family_fine(expr), key)

    def test_priority_and_volume_beats_cross_sectional(self):
        self.assertEqual(family_fine('Corr($close,$volume,10)+Slope($close,5)'), 'corr:s:v')
        self.assertEqual(family_fine('CSRank(Mean($volume,5))'), 'mean:s:v')

    def test_unparseable_expression_does_not_raise(self):
        self.assertEqual(family_fine('Mean($close,'), 'unparsed')

    def test_finer_than_legacy_family_and_never_other(self):
        rng = random.Random(0)
        exprs = [s['expr'] for s in zoo.seed_factors()]
        pool = list(exprs)
        for expr in list(exprs):
            exprs.extend(mutate(expr, rng, pool))
        fine = {family_fine(e) for e in exprs}
        self.assertGreaterEqual(len(fine), 12)
        self.assertLessEqual(len(fine), 54)  # 6 operator groups x 3 window bands x 3 flavours
        self.assertFalse(any(k == 'other' or k.startswith('other') for k in fine))
        legacy_other = [e for e in exprs if family(e) == 'other']
        self.assertGreater(len({family_fine(e) for e in legacy_other}), 3)

    def test_legacy_family_is_unchanged(self):
        self.assertEqual(family('Corr($close,$volume,5)'), 'price_volume')
        self.assertEqual(family('Mean($close,5)'), 'other')


if __name__ == '__main__':
    unittest.main()
