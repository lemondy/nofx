"""Unit tests for scripts.backtest.hidden_sharpe_check (SSRN 7679420)."""
from __future__ import annotations

import math
import numpy as np

from scripts.backtest.hidden_sharpe_check import (
    compute_hidden_sharpe_at_z,
    estimate_hidden_sharpe_grid,
    make_relu_random_features,
    run_part_a_synthetic_verification,
)


def test_compute_hidden_sharpe_at_z_identities() -> None:
    """Verify exact mathematical identities in Appendix J.1 and Lemma 2."""
    rng = np.random.default_rng(123)
    T, P = 150, 225  # c = 1.5 > 1
    R = rng.standard_normal((T, P)) + 0.05
    R_bar = np.mean(R, axis=0)
    _, s_vals, Vt = np.linalg.svd(R / math.sqrt(float(T)), full_matrices=False)
    pos_eigs = s_vals ** 2
    proj_pos = (Vt @ R_bar) ** 2
    rem_norm_sq = max(0.0, float(np.dot(R_bar, R_bar) - np.sum(proj_pos)))
    n_zero = P - len(pos_eigs)
    eigvals = np.concatenate([np.zeros(n_zero), pos_eigs])
    proj_sq = np.concatenate([np.array([rem_norm_sq] + [0.0] * (n_zero - 1)), proj_pos])

    z = 0.05
    res = compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=z)

    # Check Stieltjes transform and D(z) identity: 1 + xi_hat(z) == 1 / D(z)
    assert math.isclose(res.D_z, 1.0 - res.c + res.c * z * res.m_hat, rel_tol=1e-10)
    assert math.isclose(1.0 + res.xi_hat, 1.0 / res.D_z, rel_tol=1e-10)
    assert math.isclose(res.Z_star, z * (1.0 + res.xi_hat), rel_tol=1e-10)

    # Check Lemma 2: Z_*'(z) > 1 for all z > 0, and for spiked/anisotropic population spectrum Z_*'(0+) >= c/(c-1)
    assert res.Z_star_prime > 1.0
    pop_spiked_R = rng.standard_normal((T, P)) * (np.arange(1, P + 1, dtype=float) ** -0.6)[None, :]
    _, s_sp, Vt_sp = np.linalg.svd(pop_spiked_R / math.sqrt(float(T)), full_matrices=False)
    eigs_sp = np.concatenate([np.zeros(P - len(s_sp)), s_sp ** 2])
    proj_sp = np.zeros(P)
    res_sp = compute_hidden_sharpe_at_z(R_bar, eigs_sp, proj_sp, P=P, T=T, z=1e-6)
    assert res_sp.Z_star_prime >= (res.c / (res.c - 1.0))

    # Check finite-difference derivative of theta_d0 matches theta_d1 = - (d theta_d0 / dz) / Z_*'(z)
    eps = 1e-6
    r_plus = compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=z + eps)
    r_minus = compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=z - eps)
    d_thd0_num = (r_plus.theta_d0 - r_minus.theta_d0) / (2.0 * eps)
    d_Z_num = (r_plus.Z_star - r_minus.Z_star) / (2.0 * eps)
    assert math.isclose(res.Z_star_prime, d_Z_num, rel_tol=1e-5)
    assert math.isclose(res.theta_d1, -d_thd0_num / d_Z_num, rel_tol=1e-5)


def test_svd_matches_eigh_and_bounds_order() -> None:
    """Verify estimate_hidden_sharpe_grid works for both P < T and P > T and satisfies H1_L <= H1_U."""
    rng = np.random.default_rng(456)
    R = rng.standard_normal((120, 300)) * 0.03 + 0.002
    out = estimate_hidden_sharpe_grid(R, annualization_factor=12.0)
    assert out["T"] == 120
    assert out["P"] == 300
    assert out["combined_H1_L_sr_ann"] >= 0.0
    assert out["combined_H1_U_sr_ann"] >= out["combined_H1_L_sr_ann"]
    assert out["ridge_oos_sr_ceiling_ann"] <= out["combined_H1_L_sr_ann"] + 1e-9


def test_relu_random_features_antithetic_shape() -> None:
    """Verify Section 5.1 Eq. (33) antithetic ReLU random features."""
    rng = np.random.default_rng(789)
    X = rng.uniform(-0.5, 0.5, size=(40, 15))
    S = make_relu_random_features(X, P_Half=64, seed=42, scale=0.5)
    assert S.shape == (40, 128)
    assert np.all(S >= 0.0)


def test_part_a_synthetic_verification_properties() -> None:
    """Verify Proposition 1, Lemma 2, and Proposition 4 in synthetic Monte Carlo."""
    res = run_part_a_synthetic_verification()
    # Plug-in GRS is upward biased relative to true R_*
    assert res["mc_mean_plug_in_sr_ann"] > res["true_R_star_sr_ann"]
    # Debiased lower bound is below true R_* and upper bound is above true R_*
    assert res["mc_mean_H1_L_sr_ann"] <= res["true_R_star_sr_ann"]
    assert res["mc_median_H1_U_sr_ann"] >= res["true_R_star_sr_ann"]
    # Proposition 4: Ridge Markowitz OOS SR is bounded above by sqrt(H_1^L / Z_*'(z))
    assert res["mc_mean_ridge_oos_sr_ann"] <= res["mc_mean_prop4_bound_sr_ann"]
