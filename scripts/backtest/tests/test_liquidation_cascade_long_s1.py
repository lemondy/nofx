"""Synthetic-only H2b tests. Never fetch or read real market inputs."""
from argparse import Namespace
from contextlib import ExitStack, redirect_stderr
from datetime import timedelta
import io
import json
import math
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import numpy as np
import pandas as pd

from scripts.backtest import liquidation_cascade_long_s1 as s

T = s.ms('2022-01-01')


def bars(t=T, hours=30, price=100.):
    return pd.DataFrame(dict(open=price,high=price,low=price,close=price),
                        index=np.arange(t,t+hours*s.HOUR,s.STEP,dtype=np.int64))


def event(symbol='AUSDT',t=T):
    return dict(symbol=symbol,t=t)


def good_stats(t=3.):
    return dict(P=dict(N=100,mean=.01,SE=.003,t=t),H1=.01,H2=.01,
                stress=.005,A=.01,placebo_pct=.95,portfolio_return=.01)


def fake_plan(daily):
    with patch.object(s,'select_pools',return_value=({m:['TESTUSDT','币USDT'] for m in s.MONTHS},
                                                  {'TESTUSDT.json':'a'*64})):
        return s.make_plan(daily)


class PlanTests(unittest.TestCase):
    def test_plan_2022_no_b_btc_all_year_non_ascii_no_replacement(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            # Actual selector, sufficient listing age and no future turnover.
            days=list(s.dates('2021-12-01','2023-01-01'))
            for symbol,volume in [('TESTUSDT',1),('币USDT',2)]:
                s.write_json(root/f'{symbol}.json',[[d.isoformat(),100,100,100,100,volume,100] for d in days])
            plan=s.make_plan(root)
        self.assertEqual(sorted(plan['pools']),s.MONTHS)
        self.assertEqual(plan['p_range'],[s.P_START,s.P_END])
        self.assertNotIn('b_range',plan)
        self.assertEqual(plan['pools']['2022-01'],['币USDT','TESTUSDT'])
        self.assertEqual(plan['unfetchable'],{m:['币USDT'] for m in s.MONTHS})
        self.assertFalse(any(r['symbol']=='币USDT' for r in plan['files']))
        btc=[r for r in plan['files'] if r['symbol']=='BTCUSDT']
        for month in s.MONTHS:
            self.assertTrue(any(r['kind']=='klines5m' and r['period']==month for r in btc))
            self.assertTrue(any(r['kind']=='metrics' and r['period']==month+'-01' for r in btc))
        daily=[r['period'] for r in btc if r['kind']=='klines5m' and len(r['period'])==10]
        self.assertEqual(daily,['2023-01-01','2023-01-02'])
        self.assertEqual(min(r['period'] for r in btc if r['kind']=='metrics'),'2021-12-24')
        self.assertTrue(any(r['period']=='2021-12' and r['kind']=='klines5m' for r in btc))
        self.assertEqual(len({r['path'] for r in plan['files']}),len(plan['files']))

    def test_daily_next_two_days_only_when_next_month_archive_absent(self):
        pools={m:[] for m in s.MONTHS}
        pools['2022-01']=['TESTUSDT']
        pools['2022-03']=['TESTUSDT']  # February is March's warmup: no February dailies
        with patch.object(s,'select_pools',return_value=(pools,{})):
            plan=s.make_plan('/synthetic')
        daily=[r['period'] for r in plan['files'] if r['symbol']=='TESTUSDT' and len(r['period'])==10 and r['kind']=='klines5m']
        self.assertEqual(daily,['2022-04-01','2022-04-02'])

    def test_load_rejects_sha_path_url_duplicates_and_bad_months(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            plan=fake_plan(root)
            s.write_json(root/'plan.json',plan)
            self.assertEqual(s.load_p_plan(root),plan)
            for mutation in ('prereg','module','path','url','duplicate','month','daily_hash','missing_file'):
                bad=json.loads(json.dumps(plan))
                if mutation=='prereg': bad['prereg_sha256']='bad'
                if mutation=='module': bad['s0_module_sha256']='bad'
                if mutation=='path': bad['files'][0]['path']='../escape.zip'
                if mutation=='url': bad['files'][0]['url']='https://evil.example/data.zip'
                if mutation=='duplicate': bad['files'].append(bad['files'][0])
                if mutation=='month': bad['pools']['2023-01']=[]
                if mutation=='daily_hash': bad['daily_sha256']={'../bad.json':'a'*64}
                if mutation=='missing_file': bad['files'].pop()
                s.write_json(root/'plan.json',bad)
                with self.subTest(mutation=mutation),self.assertRaises(ValueError): s.load_p_plan(root)

    def test_every_command_checks_frozen_files_before_work(self):
        s.check_frozen()
        with tempfile.TemporaryDirectory() as tmp:
            bad=Path(tmp)/'bad'; bad.write_text('synthetic')
            with self.assertRaisesRegex(ValueError,'H2b prereg'): s.check_frozen(prereg=bad)
            with self.assertRaisesRegex(ValueError,'S0 module'): s.check_frozen(s0_module=bad)
            for name in ('PREREG','S0_MODULE'):
                for command in ('plan','fetch','run'):
                    with self.subTest(name=name,command=command),patch.object(s,name,bad),redirect_stderr(io.StringIO()):
                        with self.assertRaises(SystemExit) as e: s.main([command,'--p-dir',str(Path(tmp)/'p')])
                        self.assertEqual(e.exception.code,1)
            self.assertFalse((Path(tmp)/'p').exists())

    def test_plan_and_fetch_never_alias_s0(self):
        with tempfile.TemporaryDirectory() as tmp:
            for command in ('plan','fetch'):
                with patch.object(s,'fetch_plan') as fetch,redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit): s.main([command,'--s0-dir',tmp,'--p-dir',tmp])
                    fetch.assert_not_called()

    def test_coverage_weights_only_pool_hours_excludes_btc_and_warmup(self):
        pools={'2022-01':['AUSDT','币USDT'],'2022-02':['AUSDT']}
        oi=pd.Series(100.,index=np.arange(T-s.DAY,s.ms('2022-02-01'),s.HOUR))
        rows=s.coverage_rows(oi,'AUSDT',pools)
        self.assertEqual([r['valid'] for r in rows],[744,0])
        self.assertEqual([r['expected'] for r in rows],[744,672])
        self.assertEqual(s.coverage_rows(oi,'BTCUSDT',pools),[])
        self.assertEqual(s.coverage_rows(oi,'币USDT',pools),[])
        self.assertAlmostEqual(s.metrics_coverage(rows)['coverage'],744/(744+672))
        self.assertFalse(s.metrics_coverage(rows)['passed'])


class TradeTests(unittest.TestCase):
    def compare(self,b,reason='time',slip=s.SLIP):
        ref=s.arithmetic_trade(b,T,slip=slip)
        actual=s.backtrader_trade(b,T,slip=slip)
        self.assertEqual(actual['exit_reason'],reason)
        self.assertLessEqual(abs(actual['R_net']-ref['R_net']),1e-9)
        return actual

    def test_identifiable_open_entry_exit_and_fixed_slippage_outside_range(self):
        b=bars()
        b.loc[T+s.STEP,['open','high','low','close']]=[110,110,110,110]
        b.loc[T+s.STEP+4*s.HOUR,['open','high','low','close']]=[123,123,123,123]
        for slip in (s.SLIP,s.STRESS_SLIP):
            a=self.compare(b,slip=slip)
            self.assertEqual(a['entry_time'],T+s.STEP)
            self.assertEqual(a['exit_time'],T+s.STEP+4*s.HOUR)
            self.assertAlmostEqual(a['entry_price'],110*(1+slip))
            self.assertGreater(a['entry_price'],110)
            self.assertAlmostEqual(a['exit_price'],123*(1-slip))
            self.assertAlmostEqual(a['entry_fee'],a['entry_price']*s.FEE)
            self.assertAlmostEqual(a['exit_fee'],a['exit_price']*s.FEE)

    def test_intrabar_stop(self):
        b=bars(); b.loc[T+3*s.STEP,'low']=79
        a=self.compare(b,'stop')
        self.assertAlmostEqual(a['exit_price'],a['entry_price']*.8*(1-s.SLIP))

    def test_gap_open_stop(self):
        b=bars(); b.loc[T+3*s.STEP,['open','low']]=[70,60]
        a=self.compare(b,'stop')
        self.assertAlmostEqual(a['exit_price'],70*(1-s.SLIP))

    def test_a5_entry_bar_stop_not_delayed(self):
        b=bars(); b.loc[T+s.STEP,'low']=70
        a=self.compare(b,'a5_stop')
        self.assertEqual(a['entry_time'],a['exit_bar_time'])
        self.assertEqual(a['exit_time'],a['entry_time']+s.STEP)
        self.assertAlmostEqual(a['exit_price'],a['entry_price']*.8*(1-s.SLIP))

    def test_gap_last_close_and_missing_entry_unfilled(self):
        b=bars(); b.loc[T+2*s.STEP,'close']=105
        b.loc[T+2*s.STEP,'high']=105
        b=b.drop(T+3*s.STEP)
        a=self.compare(b,'data_gap')
        self.assertEqual(a['exit_bar_time'],T+2*s.STEP)
        self.assertEqual(a['exit_time'],T+3*s.STEP)
        self.assertAlmostEqual(a['exit_price'],105*(1-s.SLIP))
        missing=b.drop(T+s.STEP)
        a=s.backtrader_trade(missing,T)
        self.assertEqual(a['exit_reason'],'unfilled'); self.assertIsNone(a['R_net'])
        self.assertEqual(a['unfilled_reason'],'entry_bar_missing')
        a=s.backtrader_trade(b.drop(T),T)
        self.assertEqual(a['unfilled_reason'],'signal_bar_missing')

    def test_end_of_file_is_gap_and_gap_at_time_exit(self):
        b=bars().loc[:T+2*s.STEP]
        a=self.compare(b,'data_gap')
        self.assertEqual(a['exit_bar_time'],T+2*s.STEP)
        self.compare(bars().drop(T+49*s.STEP),'data_gap')

    def test_cost_formulas_zero_adverse_and_horizons(self):
        b=bars()
        b.loc[T+s.STEP,'high']=120
        b.loc[T+49*s.STEP,['open','high','low','close']]=[110]*4
        for slip in (s.SLIP,s.STRESS_SLIP):
            a=self.compare(b,slip=slip)
            expected=math.log(110*(1-slip)*(1-s.FEE)/(100*(1+slip)*(1+s.FEE)))
            self.assertAlmostEqual(a['R_net'],expected)
        self.assertAlmostEqual(s.arithmetic_trade(b,T,fee=0,slip=0)['R_net'],math.log(1.1))
        adverse=s.arithmetic_trade(b,T,adverse=True)
        self.assertAlmostEqual(adverse['R_net'],math.log(110*(1-s.SLIP)*(1-s.FEE)/(120*(1+s.FEE))))
        self.assertEqual(s.arithmetic_trade(b,T,horizon=12)['exit_time'],T+s.STEP+12*s.HOUR)
        self.assertEqual(s.arithmetic_trade(b,T,horizon=24)['exit_time'],T+s.STEP+24*s.HOUR)

    def test_time_exit_uses_open_not_intrabar_stop_on_exit_bar(self):
        b=bars(); b.loc[T+49*s.STEP,'low']=1.
        self.compare(b,'time')

    def test_reference_mismatch_and_nan_are_invalid(self):
        b=bars()
        a=s.run_engine([event()],{'AUSDT':s.sparse_windows(b,[T])})
        for value in (1.,float('nan')):
            a['trades'][0]['R_net']=value
            with patch.object(s,'run_engine',return_value=a),self.assertRaises(s.InvalidStudy):
                s.backtrader_trade(b,T)

    def test_quote_volume_eighth_column_header_and_microseconds(self):
        with tempfile.TemporaryDirectory() as tmp:
            paths=[]
            for i,header in enumerate((False,True)):
                path=Path(tmp)/f'{i}.zip'; paths.append(path)
                stamp=T+i*s.STEP
                text=('open_time,open,high,low,close,volume,close_time,quote_asset_volume\n' if header else '')
                text+=f'{stamp*(1000 if header else 1)},100,100,100,100,999,{stamp+s.STEP-1},{123+i}\n'
                with zipfile.ZipFile(path,'w') as z: z.writestr('bars.csv',text)
            q=s.read_quote_volume(paths)
            self.assertEqual(q.tolist(),[123.,124.])

    def test_ex_btc_actual_window_and_gap_close(self):
        b=bars(); b.loc[T+49*s.STEP,['open','high','close']]=110
        trade=dict(s.arithmetic_trade(b,T),R_net_main=.02)
        self.assertAlmostEqual(s.btc_adjustment(b,trade),.02-math.log(1.1))
        trade['exit_reason']='data_gap'; b.loc[trade['exit_time'],'close']=105
        self.assertAlmostEqual(s.btc_adjustment(b,trade),.02-math.log(1.05))
        self.assertIsNone(s.btc_adjustment(b.drop(T+3*s.STEP),trade))


class PortfolioTests(unittest.TestCase):
    def run_pair(self,events,frames,start=s.P_START,end=s.P_END):
        sparse={k:s.sparse_windows(f,[e['t'] for e in events if e['symbol']==k]) for k,f in frames.items()}
        a=s.run_engine(events,sparse,portfolio=True,start=start,end=end)
        r=s.arithmetic_portfolio(events,sparse,start,end)
        self.assertTrue(s.reconcile_portfolio(a,r)['valid'])
        a['reconciliation']=s.reconcile_portfolio(a,r)
        return a,r

    def test_ten_percent_ten_pending_cap_alpha_order_and_skip(self):
        symbols=[f'T{i:02}USDT' for i in range(11)]
        frames={symbol:bars() for symbol in symbols}
        # Reservations include costs even when later falling opens release cash.
        for f in frames.values(): f.loc[T+s.STEP:]=99.
        a,r=self.run_pair([event(x) for x in reversed(symbols)],frames)
        self.assertEqual([r['symbol'] for r in a['accepted']],symbols[:10])
        self.assertEqual(a['skipped'],1); self.assertEqual(a['max_concurrent'],10)
        self.assertEqual(len(a['trades']),10)
        self.assertEqual(a['cash_capped'],1); self.assertEqual(a['cash_skipped'],0)
        for row in a['accepted'][:9]:
            self.assertAlmostEqual(row['size']*row['order_close'],row['order_equity']*.1)
        last=a['accepted'][-1]
        self.assertTrue(last['cash_capped'])
        self.assertAlmostEqual(last['order_free_cash'],s.CAPITAL-sum(x['reserved_cash'] for x in a['accepted'][:9]))
        self.assertAlmostEqual(sum(x['reserved_cash'] for x in a['accepted']),s.CAPITAL)

    def test_ten_simultaneous_one_percent_gap_records_overdraft_without_invalid(self):
        events=[event(f'T{i:02}USDT') for i in range(10)]
        frames={e['symbol']:bars() for e in events}
        for frame in frames.values(): frame.loc[T+s.STEP:]=101.
        a,r=self.run_pair(events,frames)
        self.assertEqual(a['errors'],[])
        self.assertEqual(len(a['trades']),10)
        self.assertEqual(a['cash_capped'],1)
        self.assertEqual(a['cash_skipped'],0)
        self.assertEqual(a['real_cash_negative_count'],1)
        self.assertAlmostEqual(a['real_cash_min'],-100.)
        self.assertAlmostEqual(a['real_cash_max_overdraft'],100.)
        metrics=s.portfolio_metrics(a,s.P_START,s.P_END)
        for key in ('cash_capped','cash_skipped','real_cash_min','real_cash_negative_count','real_cash_max_overdraft'):
            self.assertEqual(metrics[key],a[key])
        self.assertAlmostEqual(metrics['total_return'],a['daily'][-1]['equity']/s.CAPITAL-1)
        self.assertAlmostEqual(metrics['max_dd'],1-a['daily'][-1]['equity']/s.CAPITAL)

    def test_unrealized_gains_cap_tenth_order_at_free_cash_formula(self):
        symbols=[f'T{i:02}USDT' for i in range(10)]
        events=[event(x) for x in symbols[:9]]+[event(symbols[9],T+s.HOUR)]
        frames={symbol:bars() for symbol in symbols}
        for symbol in symbols[:9]: frames[symbol].loc[T+s.HOUR:]=110.
        a,r=self.run_pair(events,frames)
        last=a['accepted'][-1]
        free=s.CAPITAL-9*10*100*(1+s.SLIP)*(1+s.FEE)
        self.assertGreater(last['order_equity'],s.CAPITAL)
        self.assertGreater(last['order_equity']*.1,free)
        self.assertAlmostEqual(last['order_free_cash'],free)
        self.assertAlmostEqual(last['size'],free/(last['order_close']*(1+s.SLIP)*(1+s.FEE)))
        self.assertTrue(last['cash_capped'])
        self.assertEqual(a['cash_capped'],1)
        self.assertEqual(a['cash_skipped'],0)

    def test_exhausted_cash_skips_below_ten_positions_separately_from_full(self):
        symbols=[f'T{i:02}USDT' for i in range(10)]
        events=[event(x) for x in symbols[:9]]+[event(symbols[9],T+s.HOUR)]
        # Cover a small positive remainder below 0.1% equity and an overdraft.
        for entry in (110.87,111.):
            with self.subTest(entry=entry):
                frames={symbol:bars() for symbol in symbols}
                for symbol in symbols[:9]: frames[symbol].loc[T+s.STEP:]=entry
                a,r=self.run_pair(events,frames)
                if entry==110.87: self.assertGreater(a['real_cash_min'],0.)
                else: self.assertLess(a['real_cash_min'],0.)
                self.assertEqual(len(a['accepted']),9)
                self.assertEqual(a['max_concurrent'],9)
                self.assertEqual(a['cash_skipped'],1)
                self.assertEqual(a['skipped'],0)
                self.assertEqual(a['cash_capped'],0)
                metrics=s.portfolio_metrics(a,s.P_START,s.P_END)
                self.assertEqual(metrics['cash_skipped'],1)
                self.assertEqual(metrics['skipped_full'],0)

    def test_intrabar_overdraft_survives_same_bar_a5_cash_recovery(self):
        events=[event(f'T{i:02}USDT') for i in range(10)]
        frames={e['symbol']:bars() for e in events}
        for frame in frames.values():
            frame.loc[T+s.STEP:]=101.
            frame.loc[T+s.STEP,'low']=70.
        a,r=self.run_pair(events,frames)
        self.assertTrue(all(row['exit_reason']=='a5_stop' for row in a['trades']))
        self.assertAlmostEqual(a['real_cash_min'],-100.)
        self.assertEqual(a['real_cash_negative_count'],1)
        self.assertAlmostEqual(a['real_cash_max_overdraft'],100.)
        self.assertGreater(a['observations'][1][1],0.)

    def test_buffer_excluded_from_initial_daily_equity_and_metrics(self):
        frames={'AUSDT':bars(hours=24)}
        a=s.run_engine([],frames,portfolio=True,end='2022-01-02')
        r=s.arithmetic_portfolio([],frames,end='2022-01-02')
        a['reconciliation']=s.reconcile_portfolio(a,r)
        self.assertTrue(all(value==s.CAPITAL for _,value in a['observations']))
        self.assertEqual(a['daily'][0]['equity'],10000.)
        metrics=s.portfolio_metrics(a,s.P_START,'2022-01-02')
        self.assertEqual(metrics['total_return'],0.)
        self.assertEqual(metrics['max_dd'],0.)
        self.assertEqual(metrics['cash_buffer'],s.CASH_BUFFER)
        a,r=self.run_pair([],{},end='2022-01-02')
        self.assertEqual(a['real_cash_min'],10000.)
        single=s.run_engine([event()],{'AUSDT':s.sparse_windows(bars(),[T])})
        self.assertEqual(single['accepted'][0]['size'],1.)
        self.assertEqual(single['accepted'][0]['order_equity'],1e9)
        self.assertEqual(single['observations'][0][1],1e9)

    def test_sparse_prenext_stale_stop_guard_gap_a5_and_multi_day(self):
        frames={'AUSDT':bars(), 'BUSDT':bars(T+s.DAY), 'CUSDT':bars(T+s.HOUR)}
        frames['AUSDT'].loc[T+s.STEP,'low']=70
        frames['BUSDT']=frames['BUSDT'].drop(T+s.DAY+3*s.STEP)
        events=[event(),event('BUSDT',T+s.DAY),event('CUSDT',T+s.HOUR)]
        a,r=self.run_pair(events,frames,end='2022-01-04')
        self.assertEqual([r['exit_reason'] for r in a['trades']],['a5_stop','time','data_gap'])
        self.assertEqual(len(a['daily']),3)
        m=s.portfolio_metrics(a,s.P_START,'2022-01-04')
        self.assertGreater(m['holding_fraction'],0)
        self.assertGreater(m['max_dd'],0)
        self.assertIn('weekly',m)

    def test_time_exit_gap_stop_and_day_rollover_reconcile(self):
        t=s.ms('2022-01-01')+22*s.HOUR
        f=bars(t)
        a,r=self.run_pair([event(t=t)],{'AUSDT':f},end='2022-01-03')
        self.assertEqual(len(a['daily']),2)
        self.assertNotEqual(a['daily'][0]['equity'],s.CAPITAL)
        self.assertAlmostEqual(a['daily'][1]['equity'],r['daily'][1]['equity'])
        f.loc[t+2*s.STEP,'low']=70
        self.run_pair([event(t=t)],{'AUSDT':f},end='2022-01-03')

    def test_abnormal_gap_exhausting_hidden_buffer_remains_invalid(self):
        frame=bars(); frame.loc[T+s.STEP:]=200000.
        frames={'AUSDT':s.sparse_windows(frame,[T])}
        a=s.run_engine([event()],frames,portfolio=True)
        self.assertTrue(any('Margin' in e for e in a['errors']))
        with self.assertRaises(s.InvalidStudy): s.reconcile_portfolio(a,{})
        with self.assertRaisesRegex(s.InvalidStudy,'缓冲现金'): s.arithmetic_portfolio([event()],frames)

    def test_reconciliation_mismatch_is_invalid(self):
        a,r=self.run_pair([event()],{'AUSDT':bars()})
        for key in ('cash_capped','cash_skipped','real_cash_negative_count','real_cash_min','real_cash_max_overdraft'):
            for change in (1.,float('nan')):
                with self.subTest(key=key,change=change):
                    with self.assertRaises(s.InvalidStudy): s.reconcile_portfolio(dict(a,**{key:a[key]+change}),r)
        # A small cash minimum still uses relative, rather than absolute, tolerance.
        with self.assertRaisesRegex(s.InvalidStudy,'real_cash_min'):
            s.reconcile_portfolio(dict(a,real_cash_min=.01+1e-10),dict(r,real_cash_min=.01))
        a['daily'][0]['equity']+=1
        with self.assertRaises(s.InvalidStudy): s.reconcile_portfolio(a,r)
        a['daily'][0]['equity']=float('nan')
        with self.assertRaises(s.InvalidStudy): s.reconcile_portfolio(a,r)


class StatisticsTests(unittest.TestCase):
    def test_cr1_mean_se_hand_calculation(self):
        values=[1.,2.,4.,8.]
        times=[T,T+s.HOUR,T+s.DAY,T+s.DAY+s.HOUR]
        avg=3.75
        expected=math.sqrt(2*( ((1-avg)+(2-avg))**2+((4-avg)+(8-avg))**2 )/16)
        row=s.cr1_mean(values,times)
        self.assertEqual(row['mean'],avg); self.assertAlmostEqual(row['SE'],expected)
        self.assertAlmostEqual(row['t'],avg/expected)
        self.assertIsNone(s.cr1_mean([1,2],[T,T])['t'])
        self.assertIsNone(s.cr1_mean([1,1],[T,T+s.DAY])['t'])
        self.assertIsNone(s.cr1_mean([],[])['mean'])

    def test_verdict_every_branch_and_each_screen_condition(self):
        self.assertEqual(s.decide(good_stats(),False)['verdict'],'invalid')
        x=good_stats(); x['P']['N']=99
        self.assertEqual(s.decide(x)['verdict'],'insufficient')
        self.assertEqual(s.decide(good_stats())['verdict'],'strong')
        self.assertEqual(s.decide(good_stats(2.1))['verdict'],'screen_pass')
        for name in ('mean','t','H1','H2','stress','placebo_pct','A','portfolio_return'):
            x=good_stats()
            if name in ('mean','t'): x['P'][name]=0.
            else: x[name]=0.
            with self.subTest(name=name): self.assertEqual(s.decide(x)['verdict'],'fail')
        for v in (None,float('nan')):
            x=good_stats(); x['P']['t']=v
            self.assertEqual(s.decide(x)['verdict'],'fail')
        self.assertAlmostEqual(s.Z_K,2.773,places=3)
        self.assertEqual(s.decide(good_stats(s.Z_K))['verdict'],'strong')
        self.assertEqual(s.decide(good_stats(np.nextafter(s.Z_K,0.)))['verdict'],'screen_pass')

    def test_s0_subset_missing_and_unexplained(self):
        a=('AUSDT',T); b=('AUSDT',T+s.DAY)
        self.assertTrue(s.reconcile_s0([a],[a,b],{b:['24']})['passed'])
        self.assertEqual(s.reconcile_s0([a],[a,b],{b:['24']})['missing_reason_counts'],{'24':1})
        self.assertFalse(s.reconcile_s0([a,b],[a],{})['passed'])
        self.assertFalse(s.reconcile_s0([a],[a,b],{})['passed'])

    def test_event_reuses_s0_classification_and_per_class_dedup_pool_filter(self):
        times=np.array([T,T+s.HOUR,T+s.DAY,T+2*s.DAY])
        features=pd.DataFrame(dict(r_1h=[-.04]*4,sigma=[.01]*4,dOI_1h=[-.1,0.,-.1,-.1]),index=times)
        with patch.object(s,'hourly_features',return_value=features):
            f=s.event_frame(pd.DataFrame(),pd.Series(dtype=float),'AUSDT',{'2022-01':['AUSDT']},s.P_START,s.P_END)
            self.assertEqual(f.loc[times,'category'].tolist(),['C','D','C','C'])
            empty=s.event_frame(pd.DataFrame(),pd.Series(dtype=float),'BUSDT',{'2022-01':['AUSDT']},s.P_START,s.P_END)
            self.assertTrue(empty.empty)


class PlaceboTests(unittest.TestCase):
    def test_same_symbol_month_pool_full_window_and_seed(self):
        t=s.ms('2022-01-31')+18*s.HOUR
        b=bars(t,hours=15)
        b=b.drop(t+3*s.HOUR+2*s.STEP)
        pools={'2022-01':['AUSDT'],'2022-02':['BUSDT']}
        c=s.placebo_candidates(b,'AUSDT',pools)
        self.assertEqual(set(c),{('AUSDT','2022-01')})
        candidate=c[('AUSDT','2022-01')]
        for stamp,r in zip(candidate['times'],candidate['returns']):
            self.assertEqual(stamp%s.HOUR,0)
            self.assertEqual(s.utc_text(stamp)[:7],'2022-01')
            self.assertTrue(s.complete_window(b,stamp))
            self.assertAlmostEqual(r,s.arithmetic_trade(b,stamp)['R_net'])
        self.assertNotIn(t,candidate['times'])
        self.assertGreater(len(candidate['times']),0)
        events=[dict(event(t=t),R_net_main=.01)]
        a=s.placebo(events,c,.01); d=s.placebo(events,c,.01)
        self.assertEqual(a,d); self.assertEqual(len(a['means']),2000)
        self.assertEqual(a['seed'],20261112)
        self.assertEqual(s.placebo([dict(event('BUSDT',t),R_net_main=.01)],c,.01)['valid'],False)

    def test_vectorized_candidates_match_stop_a5_gap_and_time_oracle(self):
        b=bars(hours=12)
        b.loc[T+s.STEP,'low']=70
        b.loc[T+s.HOUR+2*s.STEP,['open','low']]=[60,50]
        b.loc[T+4*s.HOUR+2*s.STEP,'low']=70
        c=s.placebo_candidates(b,'AUSDT',{'2022-01':['AUSDT']})[('AUSDT','2022-01')]
        for t,r in zip(c['times'],c['returns']): self.assertAlmostEqual(r,s.arithmetic_trade(b,t)['R_net'])

    def test_no_eligible_invalid_and_exact_tie_percentile(self):
        events=[dict(event(),R_net_main=.01)]
        a=s.placebo(events,{('AUSDT','2022-01'):dict(times=[],returns=[])},0.)
        self.assertFalse(a['valid']); self.assertEqual(len(a['missing']),1)
        a=s.placebo(events,{('AUSDT','2022-01'):dict(times=[T],returns=[0.])},0.)
        self.assertEqual(a['percentile'],.5)

    def test_unfilled_excluded_from_draws_denominator_candidate_counts_and_missing(self):
        rows=[dict(event(),R_net_main=.01),dict(event(t=T+s.HOUR),R_net_main=.03),
              dict(event('BUSDT'),R_net_main=None)]
        candidates={('AUSDT','2022-01'):dict(times=[T,T+s.HOUR],returns=[.02,.04])}
        with patch.object(s.np.random,'default_rng') as factory:
            factory.return_value.integers.side_effect=[np.zeros(5,dtype=int),np.ones(5,dtype=int)]
            a=s.placebo(rows,candidates,.02,repeats=5)
            self.assertEqual(factory.return_value.integers.call_count,2)
            for call in factory.return_value.integers.call_args_list:
                self.assertEqual(call.kwargs['size'],5)
        self.assertTrue(a['valid']); self.assertEqual(a['missing'],[])
        self.assertEqual(a['filled_count'],2); self.assertEqual(a['unfilled_count'],1)
        self.assertEqual(a['candidate_counts'],{'AUSDT/2022-01':2})
        np.testing.assert_allclose(a['means'],[.03]*5)
        empty=s.placebo(rows[-1:],{},None)
        self.assertTrue(empty['valid']); self.assertEqual(empty['means'],[])
        self.assertEqual(empty['candidate_counts'],{}); self.assertEqual(empty['missing'],[])
        self.assertEqual(empty['filled_count'],0); self.assertEqual(empty['unfilled_count'],1)


class OutputLedgerTests(unittest.TestCase):
    def test_complete_synthetic_run_outputs_and_s0_reconciliation(self):
        with tempfile.TemporaryDirectory() as tmp,ExitStack() as stack:
            root=Path(tmp); daily=root/'daily'; daily.mkdir()
            s0_root,p_root=root/'s0',root/'p'
            plans={}
            for segment,data_root,start in (('P',p_root,s.P_START),('A',s0_root,s.A_START)):
                data_root.mkdir()
                stamp=s.ms(start)
                month=start[:7]
                files=[]
                for symbol in ('TESTUSDT','BTCUSDT'):
                    path=data_root/'raw'/f'{symbol}.zip'; path.parent.mkdir(exist_ok=True)
                    text=''.join(f'{int(t)},100,100,100,100,1,{int(t)+s.STEP-1},2000000\n'
                                 for t in bars(stamp).index)
                    with zipfile.ZipFile(path,'w') as z: z.writestr('bars.csv',text)
                    files.append(dict(symbol=symbol,kind='klines5m',period=month,path=f'raw/{symbol}.zip'))
                    panel_path=daily/f'{symbol}.json'
                    old=json.loads(panel_path.read_bytes()) if panel_path.exists() else []
                    date_start=pd.Timestamp(start).date()-timedelta(days=1)
                    s.write_json(panel_path,old+[[d.isoformat(),100,100,100,100,1,100]
                        for d in s.dates(date_start,date_start+timedelta(days=5))])
                plans[segment]=dict(pools={month:['TESTUSDT']},files=files,daily_sha256={})
                s.write_json(data_root/'plan.json',{})
                s.write_json(data_root/'manifest.json',{})
            frozen=s0_root/'s0_v1_corrected/events.jsonl'; frozen.parent.mkdir()
            frozen.write_bytes(s.json_bytes(dict(segment='A',category='C',symbol='TESTUSDT',t=s.ms(s.A_START))))
            stack.enter_context(patch.object(s,'load_p_plan',return_value=plans['P']))
            stack.enter_context(patch.object(s,'load_s0_plan',return_value=plans['A']))
            stack.enter_context(patch.object(s,'inspect_data',return_value=dict(passed=True,structural_failure=False)))
            oi=pd.Series(100.,index=np.arange(T,s.ms('2022-02-01'),s.HOUR))
            stack.enter_context(patch.object(s,'read_metrics',return_value=(oi,{})))
            def synthetic_frame(b,oi,symbol,pools,start,end):
                times=[s.ms(start)]
                if start==s.P_START: times.append(s.ms(start)+2*s.DAY)  # no entry bars
                return pd.DataFrame(dict(category=['C']*len(times)),index=times)
            stack.enter_context(patch.object(s,'event_frame',side_effect=synthetic_frame))
            opts=Namespace(s0_dir=s0_root,p_dir=p_root,daily_dir=daily,out=root/'out',
                           allow_data_check_fail=False,write_ledger=None)
            result=s.run_study(opts)
            self.assertEqual(result['verdict'],'insufficient')
            self.assertTrue(result['valid'])
            self.assertTrue(result['s0_reconciliation']['passed'])
            self.assertEqual(result['stats']['P']['N'],1)
            self.assertEqual(result['placebo']['filled_count'],1)
            self.assertEqual(result['placebo']['unfilled_count'],1)
            self.assertEqual(set(result['checks']),set(s.decide({})['checks']))
            rows=[json.loads(line) for line in (root/'out/trades.jsonl').read_text().splitlines()]
            self.assertEqual(len(rows),3)
            for row in rows:
                if row['R_net_main'] is None:
                    self.assertEqual(row['exit_reason'],'unfilled')
                    continue
                self.assertEqual(row['entry_quote_volume'],2000000.)
                for key in ('R_net_main','R_net_stress','R_net_zero','R_net_adverse','R_12h','R_24h','ex_btc'):
                    self.assertIsNotNone(row[key])
            saved=json.loads((root/'out/results.json').read_bytes())
            self.assertEqual(saved['CHOICES'],s.CHOICES)
            report=(root/'out/report.md').read_text()
            self.assertIn('P逐币月覆盖',report)
            self.assertIn('A组合相关性',report)
            self.assertIn('最差10笔',report)
            self.assertIn('排除 unfilled：1 笔',report)
            self.assertIn('逐笔成交后',report)
            for key in ('cash_capped','cash_skipped','real_cash_min','real_cash_negative_count','real_cash_max_overdraft'):
                self.assertIn(key,saved['portfolio']['P'])
                self.assertIn(key,report)
            with patch.object(s,'inspect_data',return_value=dict(passed=False,structural_failure=False)):
                diagnostic=s.run_study(Namespace(**dict(vars(opts),out=root/'debug',allow_data_check_fail=True)))
                self.assertEqual(diagnostic['verdict'],'invalid')
                self.assertFalse(diagnostic['valid'])
                self.assertIn('stats',diagnostic)
                with patch.object(s,'run_engine') as engine:
                    refused=s.run_study(Namespace(**dict(vars(opts),out=root/'refused')))
                    self.assertEqual(refused['verdict'],'invalid')
                    engine.assert_not_called()
            with patch.object(s,'inspect_data',return_value=dict(passed=False,structural_failure=True)),patch.object(s,'run_engine') as engine:
                refused=s.run_study(Namespace(**dict(vars(opts),out=root/'structural',allow_data_check_fail=True)))
                self.assertEqual(refused['verdict'],'invalid')
                engine.assert_not_called()

    def test_existing_output_refused_even_empty(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); out=root/'out'; out.mkdir()
            opts=Namespace(s0_dir=root/'s0',p_dir=root/'p',daily_dir=root/'daily',out=out,
                           allow_data_check_fail=False,write_ledger=None)
            with self.assertRaisesRegex(ValueError,'已存在'): s.run_study(opts)

    def test_invalid_run_outputs_no_backtest_and_nonzero(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            with patch.object(s,'run_engine') as engine:
                code=s.main(['run','--s0-dir',str(root/'s0'),'--p-dir',str(root/'p'),
                             '--daily-dir',str(root/'daily'),'--out',str(root/'out')])
                self.assertEqual(code,1); engine.assert_not_called()
            result=json.loads((root/'out/results.json').read_bytes())
            self.assertEqual(result['verdict'],'invalid')
            self.assertIn('CHOICES',result)
            self.assertTrue((root/'out/report.md').exists())
            self.assertEqual((root/'out/trades.jsonl').read_bytes(),b'')

    def test_temp_ledger_requires_k9_invalid_does_not_count(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'ledger.jsonl'
            result=dict(verdict='fail',stats=good_stats(),input_hashes={})
            with self.assertRaisesRegex(ValueError,'K=1'): s.write_ledger(result,path)
            self.assertEqual(path.read_bytes(),b'')
            invalid=s.write_ledger(dict(result,verdict='invalid'),path)
            self.assertFalse(invalid['counts_toward_k']); self.assertIsNone(invalid['K'])
            for i in range(8):
                s.evolution_ledger.append_event(path,'fq_crypto',lambda k,old:dict(track='fq_crypto',stage='S1',
                    change_type='E',idea_id='synthetic',baseline_version='none',verdict='fail'))
            row=s.write_ledger(result,path)
            self.assertEqual(row['K'],9); self.assertEqual(row['preregistered_K'],9)
            self.assertEqual(row['delta'],.01); self.assertEqual(row['trials'],1)
            with self.assertRaisesRegex(ValueError,'K=10'): s.write_ledger(result,path)
            self.assertEqual(len(s.evolution_ledger.read_rows(path)),10)


if __name__=='__main__':
    unittest.main()
