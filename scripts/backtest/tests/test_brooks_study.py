"""Offline inference/label tests. No data writes, network or order engine."""

import math
from pathlib import Path
import tempfile
import unittest

import numpy as np
import pandas as pd

from scripts.backtest.brooks import compute_features
from scripts.backtest.brooks_study import (
    BOUNDS, MARKETS, StatisticsBatch, TestSpec, bh_adjust, cluster_mean_se,
    control_percentile, event_labels, forward_labels, future_extreme,
    panel_for, pattern_masks, read_bars, shifted_control, specifications, split_mask,
)


def synthetic(n=180, start='2023-01-01', frequency='D', seed=218):
    rng = np.random.default_rng(seed)
    c = 100 * np.exp(np.cumsum(rng.normal(0, .01, n)))
    o = np.r_[100., c[:-1]]
    return pd.DataFrame(dict(open=o, high=np.maximum(o,c)+1,
        low=np.minimum(o,c)-1, close=c, volume=np.ones(n)),
        index=pd.date_range(start, periods=n, freq=frequency))


class LabelTests(unittest.TestCase):
    def test_exact_endpoints_and_tail(self):
        f = synthetic(32)
        labels = forward_labels(f, (1,3,5,10,20), np.full(len(f), 2.))
        for k in (1,3,5,10,20):
            expected = np.log(f.close.iloc[k:].to_numpy()/f.close.iloc[:-k].to_numpy())
            np.testing.assert_allclose(labels[f'return_{k}'].iloc[:-k], expected)
            np.testing.assert_allclose(labels[f'atr_{k}'].iloc[:-k], expected/(2/f.close.iloc[:-k]))
            self.assertTrue(labels[f'return_{k}'].iloc[-k:].isna().all())
            self.assertTrue(labels[f'end_{k}'].iloc[-k:].isna().all())
            self.assertEqual(labels[f'end_{k}'].iloc[0], f.index[k])

    def test_only_current_and_kth_close_determine_return(self):
        f = synthetic(30)
        original = forward_labels(f, (5,), np.ones(len(f)))
        g = f.copy()
        g.loc[g.index[1:5], 'close'] *= 2
        g.loc[g.index[6:], 'close'] *= 3
        changed = forward_labels(g, (5,), np.ones(len(f)))
        self.assertEqual(original.return_5.iloc[0], changed.return_5.iloc[0])
        g.loc[g.index[5], 'close'] *= 2
        changed = forward_labels(g, (5,), np.ones(len(f)))
        self.assertAlmostEqual(changed.return_5.iloc[0]-original.return_5.iloc[0], math.log(2))

    def test_future_extreme_excludes_signal_and_requires_complete_window(self):
        x = pd.Series([100., 1, 2, 3, 0, 9])
        result = future_extreme(x, 3, 'max')
        np.testing.assert_allclose(result.iloc[:3], [3,3,9])
        self.assertTrue(result.iloc[-3:].isna().all())

    def test_event_tail_and_ema_excludes_current(self):
        f = synthetic(30)
        features = pd.DataFrame({'ema20': f.close+100, 'atr': np.ones(len(f))}, index=f.index)
        features.loc[f.index[0], 'ema20'] = f.close.iloc[0]
        label = event_labels(f, features, 'ema_return', 1, 10)
        self.assertEqual(label.iloc[0], 0)
        self.assertTrue(label.iloc[-10:].isna().all())
        features.loc[f.index[4], 'ema20'] = f.close.iloc[4]
        self.assertEqual(event_labels(f, features, 'ema_return', 1, 10).iloc[0], 1)

    def test_bad_horizon(self):
        with self.assertRaises(ValueError):
            forward_labels(synthetic(5), (0,), np.ones(5))


class InferenceTests(unittest.TestCase):
    def test_known_date_cluster_se(self):
        y = np.array([1.,1.,3.,3.])
        d = ['2020-01-01']*2 + ['2020-01-02']*2
        self.assertAlmostEqual(cluster_mean_se(y,d), 1.)
        # Replicating highly correlated assets does not create information.
        self.assertAlmostEqual(cluster_mean_se(np.repeat(y,5),np.repeat(d,5)), 1.)
        self.assertAlmostEqual(cluster_mean_se([1,3,2,4],d), .5)

    def test_bh_reference_values_and_nan_planned_slot(self):
        np.testing.assert_allclose(bh_adjust([.01,.04,.03,.002]), [.02,.04,.04,.008])
        np.testing.assert_allclose(bh_adjust([.01,np.nan,.9]), [.03,1.,1.])
        np.testing.assert_allclose(bh_adjust([.05,.05,.05]), [.05]*3)
        self.assertEqual(len(bh_adjust([])),0)
        with self.assertRaises(ValueError):
            bh_adjust([1.01])

    def test_fft_shift_matches_exact_possible_offsets(self):
        y = np.array([1.,4.,9.,16.,25.])
        a = np.array([1.,0.,1.,0.,0.])
        # Every possible nonzero shift; seeded draws must agree exactly.
        rng = np.random.default_rng(78)
        offsets = rng.integers(1,5,size=50)
        expected = np.array([np.roll(a,k) @ y / a.sum() for k in offsets])
        np.testing.assert_allclose(shifted_control(y,a,repeats=50,seed=78), expected)

    def test_paired_control_preserves_arm_overlap(self):
        y = np.arange(100.)
        a = np.zeros(100); a[[1,20,50,71]]=1
        np.testing.assert_allclose(shifted_control(y,a,a,repeats=500,seed=2),0,atol=1e-12)
        self.assertEqual(control_percentile(0,np.zeros(500)),.5)

    def test_two_dates_have_one_legal_shift_singleton_does_not(self):
        np.testing.assert_allclose(shifted_control([2.,8.],[1.,0.],repeats=500),8.)
        self.assertTrue(np.isnan(shifted_control([2.],[1.],repeats=500)).all())

    def test_no_signal_control_percentiles_approximately_uniform(self):
        # Independent masks on independent circular IID series: null ranks
        # should fill [0,1], not be systematically 'significant'.
        rng = np.random.default_rng(20261023)
        ranks=[]
        for j in range(240):
            y=rng.normal(size=512)
            a=np.zeros(512); a[rng.choice(512,48,replace=False)]=1
            controls=shifted_control(y,a,repeats=500,seed=j)
            ranks.append(control_percentile(a @ y / a.sum(),controls))
        self.assertLess(abs(np.mean(ranks)-.5),.055)
        counts=np.histogram(ranks,bins=[0,.25,.5,.75,1])[0]
        self.assertTrue(np.all((counts>=40)&(counts<=80)),counts)
        self.assertLess(np.mean(np.asarray(ranks)>.95),.1)

    def test_batch_paired_se_and_whole_date_bootstrap(self):
        dates=pd.to_datetime(['2023-01-01']*2+['2023-01-02']*2)
        p=pd.DataFrame(dict(date=dates,symbol=[0]*4,y=[1.,1,3,3],raw=np.array([1.,1,3,3])/10000,
            atr=[.01]*4,scale_inverse=[100.]*4,all=[True]*4))
        batch=StatisticsBatch(dates,21)
        batch.add(p,TestSpec('T0','drift',1,1,arm_a='all',arm_b='constant'),'us_daily','train')
        row=batch.finish()[0]
        self.assertAlmostEqual(row['mean'],2.)
        self.assertAlmostEqual(row['se'],1.)
        self.assertAlmostEqual(row['ci_low'],1.)
        self.assertAlmostEqual(row['ci_high'],3.)
        self.assertEqual(row['p'],1.)  # too few date clusters

    def test_identical_overlapping_arms_have_zero_contrast_se(self):
        dates=pd.date_range('2023-01-01',periods=40)
        raw=np.linspace(-.01,.02,40)
        p=pd.DataFrame(dict(date=dates,symbol=[0]*40,y=raw,raw=raw,
            atr=raw*100,scale_inverse=[100.]*40,all=[True]*40))
        batch=StatisticsBatch(dates,31)
        batch.add(p,TestSpec('T0','paired',1,1,arm_a='all',arm_b='all'),'us_daily','train')
        r=batch.finish()[0]
        self.assertEqual(r['effect'],0.)
        self.assertEqual(r['se'],0.)
        self.assertEqual(r['ci_low'],0.)
        self.assertEqual(r['ci_high'],0.)
        self.assertEqual(r['p'],1.)


class SplitAndSignalTests(unittest.TestCase):
    def test_exact_boundaries_and_endpoint_purge(self):
        index=pd.to_datetime(['2023-12-30','2023-12-31','2024-01-01','2024-12-31','2025-01-01'])
        np.testing.assert_array_equal(split_mask(index,'train'),[1,1,0,0,0])
        np.testing.assert_array_equal(split_mask(index,'validation'),[0,0,1,1,0])
        np.testing.assert_array_equal(split_mask(index,'holdout'),[0,0,0,0,1])
        endpoints=index+pd.Timedelta(days=1)
        np.testing.assert_array_equal(split_mask(index,'train',endpoints),[1,0,0,0,0])
        np.testing.assert_array_equal(split_mask(index,'validation',endpoints),[0,0,1,0,0])

    def test_hourly_purge_at_year_boundary(self):
        index=pd.date_range('2024-12-31 22:00',periods=4,freq='h')
        endpoint=index+pd.Timedelta(hours=1)
        np.testing.assert_array_equal(split_mask(index,'validation',endpoint),[1,0,0,0])
        np.testing.assert_array_equal(split_mask(index,'holdout',endpoint),[0,0,1,1])

    def test_train_panel_cannot_access_validation_labels(self):
        f=synthetic(150,start='2023-10-01')
        features=compute_features(f)
        members=[dict(symbol='TEST',frame=f,features=features,eligible=np.ones(len(f),bool))]
        original=panel_for(members,'crypto_daily','train',5,1,2)
        # Changes beyond the training boundary cannot affect any training label.
        f2=f.copy(); f2.loc[f2.index>='2024-01-01','close']*=100
        members[0]['frame']=f2
        changed=panel_for(members,'crypto_daily','train',5,1,2)
        pd.testing.assert_frame_equal(original,changed)
        self.assertLess(original.date.max(),pd.Timestamp('2023-12-27'))

    def test_h2_attributes_are_from_previous_signal_bar(self):
        frame=synthetic(70)
        f=compute_features(frame)
        f.loc[:,'h2']=False
        f.loc[f.index[50],'h2']=True
        f.loc[:,'always_in']=1
        f.loc[:,'reversal_bull_quality_score']=0
        f.loc[f.index[49],'reversal_bull_quality_score']=3
        masks=pattern_masks(frame,f,1,3)
        self.assertTrue(masks['t4_c'][50])
        self.assertFalse(masks['t4_c'][49])
        self.assertFalse(masks['t4_c'][51])

    def test_preregistered_grid_is_unique_and_equal_across_markets(self):
        lengths=[]
        for market in MARKETS:
            specs=specifications(market)
            self.assertEqual(len({s.key for s in specs}),len(specs))
            self.assertEqual({s.test for s in specs},{'T0','T1','T2','T3','T4','T5','T6'})
            lengths.append(len(specs))
        self.assertEqual(lengths,[352]*3)

    def test_reader_ignores_old_hourly_schema_and_unclosed_bar(self):
        import json
        # Test outputs go to OS temp only, never the read-only data tree.
        with tempfile.TemporaryDirectory(dir='/private/tmp') as tmp:
            path=Path(tmp)/'TEST.json'
            path.write_text(json.dumps([[0,10,11,9,10,1], [3600000,10,11,9,10,1,7],
                                        [7200000,10,11,9,10,1]]))
            f,rejected=read_bars(path,True,pd.Timestamp('1970-01-01 02:30'))
            self.assertEqual(len(f),1)
            self.assertEqual(rejected,2)


if __name__=='__main__':
    unittest.main()
