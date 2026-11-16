#!/usr/bin/env python3
"""Hidden Sharpe Ratio (Chernov, Kelly, Malamud & Schwab, 2026, SSRN 7679420)
Local Verification, Diagnostic Audit on nofx Data, and Backtest Counterfactual Check.

Read-only diagnostic script — does NOT modify data/, evolution_ledger.json, or K budget.

Parts:
  A. Analytical J=1,2 Higher-Order Debiased GRS Estimator + Appendix M.1 Monte Carlo verification
     - Verifies Proposition 1 / Theorem 2 (debiased GRS & higher-order B_j recovery)
     - Verifies Lemma 2 (Z_*'(0) >= c / (c - 1) for c > 1)
     - Verifies Corollary 3 & Section 4.3 (H_1^L <= R_*^2 <= H_1^U)
     - Verifies Proposition 4 (Ridge Markowitz OOS Sharpe^2 <= H_1^L / Z_*'(z) <= R_*^2 / Z_*'(z))
  B. Diagnostic Audit on nofx Real Data:
     - B1: Crypto 15-leg managed portfolios (msrr_study universe, train & CV periods)
     - B2: Crypto 45-feature linear vs ReLU Random Feature (P=500, 2000) managed portfolios
     - B3: US Equity bStock (mega/large cap) vs S&P 500 broad linear & ReLU RF managed portfolios
  C. Counterfactual Portfolio Construction Check (Walk-Forward OOS):
     - Compares Equal-Weight / Baseline vs Standard Ridge MSRR vs Higher-Order Neumann-Debiased
       Ridge Portfolio w_HO(z) = (zI + Sigma_hat)^{-1} R_bar + Z_*(z) (zI + Sigma_hat)^{-2} R_bar
       and PC-Defactored Ridge to test whether small-eigenvalue restoration helps OOS portfolio weights.
"""

from __future__ import annotations

import json
import math
import sys
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Dict, List, Optional, Sequence, Tuple

import numpy as np
import pandas as pd

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


# ============================================================================
# 1. Exact Closed-Form Estimator from Appendix H, I, J.1, K, L (SSRN 7679420)
# ============================================================================

@dataclass
class HiddenSharpeAtZ:
    z: float
    P: int
    T: int
    c: float
    lambda_bar: float
    theta_hat: float          # Plug-in ridge GRS: R_bar' (zI + Sigma_hat)^{-1} R_bar
    m_hat: float              # Stieltjes transform: P^{-1} tr((zI + Sigma_hat)^{-1})
    D_z: float                # Denominator 1 - c + c z m_hat(-z)
    xi_hat: float             # Bias term c (1 - z m_hat(-z)) / D(z)
    Z_star: float             # Effective regularization z (1 + xi_hat(z))
    Z_star_prime: float       # dZ_*(z)/dz (Limits-to-Learning multiplier; gap is Z_*'(z) - 1)
    Z_star_prime2: float      # d^2 Z_*(z)/dz^2
    theta_d0: float           # Debiased 0th-order GRS: (theta_hat - xi_hat) / (1 + xi_hat)
    theta_d1: float           # Debiased 1st-order derivative estimator of B'(Z_*)
    theta_0: float            # B_0hat = theta_d0
    theta_1: float            # B_1hat = Z_* * theta_d1
    theta_2: float            # B_2hat = Z_*^2 * theta_d2
    D_1: float                # Partial sum theta_0 + theta_1
    H1_L: float               # J=1 Lower bound on R_*^2 (clamped >= 0)
    a_min_1: float            # Optimal subdiagonality threshold a_{min, 1}
    a_corner: float           # Corner threshold for subdiagonality validation
    a_star_over_lambda_bar: float
    H1_U: float               # J=1 Upper bound on R_*^2
    ridge_oos_sr2_bound: float  # Prop 4 upper bound on Ridge OOS SR^2: H1_L / Z_*'(z)
    se_theta0: float          # Pivotal CLT SE for theta_0
    se_theta1: float          # Pivotal CLT SE for theta_1


def compute_hidden_sharpe_at_z(
    R_bar: np.ndarray,
    eigvals: np.ndarray,
    proj_sq: np.ndarray,
    P: int,
    T: int,
    z: float,
) -> HiddenSharpeAtZ:
    """Compute exact closed-form statistics at penalty z using Appendix J.1 & I & K.

    Args:
        R_bar: (P,) sample mean vector
        eigvals: (P,) eigenvalues of sample covariance Sigma_hat = R'R/T - R_bar R_bar'
        proj_sq: (P,) squared projections (u_i' R_bar)^2 onto eigenvectors of Sigma_hat
        P: number of managed portfolios / assets
        T: number of time-series observations
        z: ridge penalty > 0
    """
    c = float(P) / float(T)
    lambda_bar = float(np.mean(eigvals))
    inv_zp = 1.0 / (z + eigvals)

    # Power sums S_k(z) = P^{-1} sum (z + s_i)^{-k}, T_k(z) = sum proj_sq_i (z + s_i)^{-k}
    S = [0.0]  # 1-indexed up to k=5
    T_sum = [0.0]
    for k in range(1, 6):
        inv_k = inv_zp ** k
        S.append(float(np.mean(inv_k)))
        T_sum.append(float(np.sum(proj_sq * inv_k)))

    # Step 2: Value and derivatives of m_hat(-z) and Theta_hat(z) (Eq. J.3)
    # m^{(n)}(z) = (-1)^n n! S_{n+1}(z), Theta_hat^{(n)}(z) = (-1)^n n! T_{n+1}(z)
    m_deriv = [((-1) ** n) * math.factorial(n) * S[n + 1] for n in range(5)]
    th_deriv = [((-1) ** n) * math.factorial(n) * T_sum[n + 1] for n in range(5)]

    m0, m1, m2, m3, m4 = m_deriv
    th0, th1, th2, th3, th4 = th_deriv

    # Step 3: Auxiliary function M(z) = z m_hat(-z) (Eq. J.4)
    # M^{(n)}(z) = z m^{(n)}(z) + n m^{(n-1)}(z)
    M0 = z * m0
    M1 = z * m1 + 1.0 * m0
    M2 = z * m2 + 2.0 * m1
    M3 = z * m3 + 3.0 * m2
    M4 = z * m4 + 4.0 * m3

    # Step 4: Denominator D(z) = 1 - c + c M(z) (Eq. J.5)
    D0 = 1.0 - c + c * M0
    D1 = c * M1
    D2 = c * M2
    D3 = c * M3
    D4 = c * M4

    # Guard against Marchenko-Pastur pole where D0 <= 0
    if abs(D0) < 1e-12:
        D0 = 1e-12 if D0 >= 0 else -1e-12

    # Step 5: Bias correction xi_hat(z) = c (1 - M(z)) / D(z) (Eq. J.6)
    # Using quotient recursion: xi^{(n)} = (N^{(n)} - sum_{k=0}^{n-1} C(n,k) xi^{(k)} D^{(n-k)}) / D0
    N_deriv = [c * (1.0 - M0), -c * M1, -c * M2, -c * M3, -c * M4]
    D_deriv = [D0, D1, D2, D3, D4]
    xi_deriv = [0.0] * 5
    for n in range(5):
        acc = N_deriv[n]
        for k in range(n):
            acc -= math.comb(n, k) * xi_deriv[k] * D_deriv[n - k]
        xi_deriv[n] = acc / D0

    xi0, xi1, xi2, xi3, xi4 = xi_deriv

    # Step 6: Effective regularization Z_*(z) = z (1 + xi_hat(z)) (Eq. J.7)
    Z0 = z * (1.0 + xi0)
    Z1 = z * xi1 + (1.0 + xi0)
    Z2 = z * xi2 + 2.0 * xi1
    Z3 = z * xi3 + 3.0 * xi2
    Z4 = z * xi4 + 4.0 * xi3

    # Step 7: Debiased GRS Theta_d(z) = (Theta_hat(z) - xi_hat(z)) / (1 + xi_hat(z)) (Eq. J.9)
    one_plus_xi = 1.0 + xi0
    if abs(one_plus_xi) < 1e-12:
        one_plus_xi = 1e-12
    U_deriv = [th_deriv[n] - xi_deriv[n] for n in range(5)]
    V_deriv = [one_plus_xi, xi1, xi2, xi3, xi4]
    thd_deriv = [0.0] * 5
    for n in range(5):
        acc = U_deriv[n]
        for k in range(n):
            acc -= math.comb(n, k) * thd_deriv[k] * V_deriv[n - k]
        thd_deriv[n] = acc / one_plus_xi

    thd0, thd1_z, thd2_z, _, _ = thd_deriv

    # Higher-order debiased GRS statistics (Eq. 21 & Corollary H.7 Eq. H.86)
    Z1_safe = Z1 if abs(Z1) > 1e-12 else 1e-12
    theta_d0 = thd0
    theta_d1 = -thd1_z / Z1_safe
    theta_d2 = (thd2_z - (Z2 / Z1_safe) * thd1_z) / (2.0 * (Z1_safe ** 2))

    theta_0 = theta_d0
    theta_1 = Z0 * theta_d1
    theta_2 = (Z0 ** 2) * theta_d2

    # Pivotal CLT covariance Sigma_Theta (Appendix H & I, Lemma I.1 - I.6)
    # A(z) = z (Theta_hat(z) - xi_hat(z)), B(z) = 3 z Theta_hat(z) - z xi_hat(z), C(z) = Z_*(z)
    A_d = [0.0] * 5
    B_d = [0.0] * 5
    C_d = [Z0, Z1, Z2, Z3, Z4]
    for n in range(5):
        # f(z) = z * g(z) => f^{(n)}(z) = z g^{(n)}(z) + n g^{(n-1)}(z)
        gA_n = th_deriv[n] - xi_deriv[n]
        gA_nm1 = (th_deriv[n - 1] - xi_deriv[n - 1]) if n >= 1 else 0.0
        A_d[n] = z * gA_n + n * gA_nm1

        gB_n = 3.0 * th_deriv[n] - xi_deriv[n]
        gB_nm1 = (3.0 * th_deriv[n - 1] - xi_deriv[n - 1]) if n >= 1 else 0.0
        B_d[n] = z * gB_n + n * gB_nm1

    A1, A2, A3, _ = A_d[1], A_d[2], A_d[3], A_d[4]
    B1, B2, B3, _ = B_d[1], B_d[2], B_d[3], B_d[4]
    C1, C2, C3, _ = C_d[1], C_d[2], C_d[3], C_d[4]
    C1_s = C1 if abs(C1) > 1e-12 else 1e-12

    # Kernel derivatives on diagonal (Eq. I.3, I.4, I.73)
    Xi_00 = 2.0 * (A1 ** 2) / C1_s + A1 + B1
    Xi_10 = -(A1 ** 2) * C2 / (C1_s ** 2) + 2.0 * A1 * A2 / C1_s + 0.5 * (A2 + B2)
    Xi_11 = (
        -(A1 ** 2) * C3 / (3.0 * (C1_s ** 2))
        + (A1 ** 2) * (C2 ** 2) / (C1_s ** 3)
        - 2.0 * A1 * A2 * C2 / (C1_s ** 2)
        + 2.0 * A1 * A3 / (3.0 * C1_s)
        + (A2 ** 2) / C1_s
        + (A3 + B3) / 6.0
    )
    # Transform by A_tilde_T for J=1: diag(1, Z0) @ [[1, 0], [0, -1/Z1]] = [[1, 0], [0, -Z0/Z1]]
    var_theta0 = max(0.0, Xi_00 / float(T))
    scale_1 = -Z0 / Z1_safe
    var_theta1 = max(0.0, (scale_1 ** 2) * Xi_11 / float(T))
    se_theta0 = math.sqrt(var_theta0)
    se_theta1 = math.sqrt(var_theta1)

    # Bounds for J=1 (Section 4.1 Corollary 3 & Appendix K)
    t0_pos = max(0.0, theta_0)
    t1_pos = max(0.0, theta_1)
    D_1 = t0_pos + t1_pos

    # Lower bound H_1^L = D_0 + theta_1 / (1 - theta_1 / theta_0) if 0 < theta_1 < theta_0
    if t0_pos > 1e-12 and 0.0 <= t1_pos < t0_pos:
        H1_L = t0_pos + t1_pos / (1.0 - t1_pos / t0_pos)
    else:
        H1_L = D_1

    # Upper bound H_1^U (Appendix K, J=1: p(u) = (1 + u)^2 = 1 + 2u + u^2)
    # Step 1: Corner threshold a_corner = Z_* * theta_1 / (theta_0 - theta_1) (Eq. K.13)
    if t0_pos > t1_pos and t1_pos > 0.0:
        a_corner = Z0 * (t1_pos / (t0_pos - t1_pos))
    else:
        a_corner = 0.0

    # Step 2: Closed-form interior minimizer a_{min, 1} for J=1 (Eq. K.10)
    # gamma = lambda_bar * B_1 / (Z_* * D_1), q(u) = (2 - gamma) u^2 + (2 - 2 gamma) u - gamma = 0
    if D_1 > 1e-12 and Z0 > 0 and lambda_bar > 0:
        gamma = (lambda_bar * t1_pos) / (Z0 * D_1)
        if 0.0 < gamma < 2.0:
            disc = 1.0 - gamma + (gamma ** 2)
            u_min = (-(1.0 - gamma) + math.sqrt(max(0.0, disc))) / (2.0 - gamma)
            a_min_1 = Z0 * max(0.0, u_min)
        else:
            a_min_1 = lambda_bar
    else:
        a_min_1 = 0.0

    a_star = max(a_min_1, a_corner)
    if a_star <= 0.0 or a_star >= lambda_bar or Z0 <= 0.0:
        H1_U = D_1 if a_star <= 0.0 else float("inf")
        a_ratio = max(0.0, min(1.0, a_star / lambda_bar if lambda_bar > 0 else 1.0))
    else:
        u_star = a_star / Z0
        p_u = (1.0 + u_star) ** 2
        a_ratio = a_star / lambda_bar
        denom_u = 1.0 - a_ratio
        H1_U = (D_1 + p_u * t1_pos) / denom_u if denom_u > 1e-9 else float("inf")

    ridge_oos_sr2_bound = H1_L / Z1_safe if Z1_safe > 0 else 0.0

    return HiddenSharpeAtZ(
        z=z,
        P=P,
        T=T,
        c=c,
        lambda_bar=lambda_bar,
        theta_hat=th0,
        m_hat=m0,
        D_z=D0,
        xi_hat=xi0,
        Z_star=Z0,
        Z_star_prime=Z1,
        Z_star_prime2=Z2,
        theta_d0=theta_d0,
        theta_d1=theta_d1,
        theta_0=theta_0,
        theta_1=theta_1,
        theta_2=theta_2,
        D_1=D_1,
        H1_L=H1_L,
        a_min_1=a_min_1,
        a_corner=a_corner,
        a_star_over_lambda_bar=a_ratio,
        H1_U=H1_U,
        ridge_oos_sr2_bound=ridge_oos_sr2_bound,
        se_theta0=se_theta0,
        se_theta1=se_theta1,
    )


def estimate_hidden_sharpe_grid(
    returns_matrix: np.ndarray,
    z_grid: Sequence[float] = (1e-4, 1e-3, 1e-2, 1e-1),
    annualization_factor: float = 12.0,
    normalize_trace: bool = True,
) -> Dict[str, object]:
    """Run Algorithm 1 + Appendix L grid combination on a (T, P) return matrix.

    Note on scaling: The quadratic form R_bar' (zI + Sigma_hat)^{-1} R_bar is scale-invariant
    ONLY if z scales with the average eigenvalue lambda_bar = tr(Sigma_hat) / P.
    When normalize_trace=True (default), returns are rescaled by 1 / sqrt(lambda_bar) so that
    lambda_bar == 1.0 and z in [1e-4, 1e-1] represents relative shrinkage z / lambda_bar.
    """
    # Paper Eq. (9) & (36) p. 12, 33: Sigma_hat = T^{-1} R' R (uncentered second moment matrix)
    T, P = returns_matrix.shape
    R_bar_raw = np.mean(returns_matrix, axis=0)
    raw_lambda_bar = float(np.sum(returns_matrix ** 2) / (float(T) * float(P)))
    if normalize_trace and raw_lambda_bar > 1e-20:
        scale_factor = 1.0 / math.sqrt(raw_lambda_bar)
        R_bar = R_bar_raw * scale_factor
        R_scaled = returns_matrix * scale_factor
    else:
        R_bar = R_bar_raw
        R_scaled = returns_matrix

    if P <= T:
        Sigma_hat = (R_scaled.T @ R_scaled) / float(T)
        eigvals, eigvecs = np.linalg.eigh(Sigma_hat)
        eigvals = np.maximum(0.0, eigvals)
        proj_sq = (eigvecs.T @ R_bar) ** 2
    else:
        # Economy SVD of R_scaled / sqrt(T): R_scaled / sqrt(T) = U S V^T, where V is (r, P) with r = min(T, P)
        _, s_vals, Vt = np.linalg.svd(R_scaled / math.sqrt(float(T)), full_matrices=False)
        pos_eigs = s_vals ** 2
        proj_pos = (Vt @ R_bar) ** 2
        rem_norm_sq = max(0.0, float(np.dot(R_bar, R_bar) - np.sum(proj_pos)))
        n_zero = P - len(pos_eigs)
        eigvals = np.concatenate([np.zeros(n_zero, dtype=float), pos_eigs])
        proj_zeros = np.zeros(n_zero, dtype=float)
        if n_zero > 0:
            proj_zeros[0] = rem_norm_sq
        proj_sq = np.concatenate([proj_zeros, proj_pos])

    grid_results = [
        compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=float(z))
        for z in z_grid
    ]

    # Appendix L: Combine bounds across z_grid
    # Filter out grid points near the Marchenko-Pastur pole where theta_1 / theta_0 -> 1
    valid_lb = [
        r for r in grid_results
        if r.D_z > 0.05 and r.Z_star > 0 and r.Z_star_prime > 0 and (r.theta_0 <= 0 or r.theta_1 / r.theta_0 < 0.85)
    ]
    if not valid_lb:
        valid_lb = [r for r in grid_results if r.D_z > 0 and r.Z_star > 0 and r.Z_star_prime > 0]
    if not valid_lb:
        valid_lb = grid_results

    best_lb_res = max(valid_lb, key=lambda r: r.H1_L)
    finite_ub = [r for r in valid_lb if math.isfinite(r.H1_U) and r.H1_U >= r.H1_L]
    best_ub_res = min(finite_ub, key=lambda r: r.H1_U) if finite_ub else best_lb_res

    ann_sqrt = math.sqrt(annualization_factor)
    combined_lb_sr = math.sqrt(max(0.0, best_lb_res.H1_L)) * ann_sqrt
    combined_ub_sr = (
        math.sqrt(max(0.0, best_ub_res.H1_U)) * ann_sqrt
        if math.isfinite(best_ub_res.H1_U)
        else float("inf")
    )
    best_ridge_oos_sr_bound = math.sqrt(max(0.0, max(r.ridge_oos_sr2_bound for r in valid_lb))) * ann_sqrt

    return {
        "T": T,
        "P": P,
        "c": float(P) / float(T),
        "raw_lambda_bar": raw_lambda_bar,
        "annualization_factor": annualization_factor,
        "combined_H1_L_sr_ann": combined_lb_sr,
        "combined_H1_U_sr_ann": combined_ub_sr,
        "best_lb_z": best_lb_res.z,
        "best_ub_z": best_ub_res.z,
        "ridge_oos_sr_ceiling_ann": best_ridge_oos_sr_bound,
        "grid": [asdict(r) for r in grid_results],
    }


def make_relu_random_features(
    X_std: np.ndarray,
    P_Half: int,
    seed: int = 42,
    scale: float = 0.5,
) -> np.ndarray:
    """Section 5.1 Eq. (33): S_t = ReLU(scale * sqrt(2/d) * X_t * Omega') with antithetic [Omega, -Omega].

    Args:
        X_std: (N, d) cross-sectionally standardized characteristics in [-0.5, 0.5]
        P_half: P // 2 Gaussian random weight draws
    Returns:
        S: (N, 2 * P_half) ReLU random feature matrix
    """
    _, d = X_std.shape
    rng = np.random.default_rng(seed)
    omega_half = rng.standard_normal((P_Half, d))
    omega = np.vstack([omega_half, -omega_half])  # (P, d)
    pre_act = scale * math.sqrt(2.0 / float(d)) * (X_std @ omega.T)
    return np.maximum(pre_act, 0.0)


# ============================================================================
# 2. Part A: Synthetic DGP & Formula Verification (Appendix M.1 & Props 1, 4)
# ============================================================================

def run_part_a_synthetic_verification() -> Dict[str, object]:
    """Verify exact properties of the estimator on known population spectra & Appendix M.1 DGP."""
    rng = np.random.default_rng(20261115)

    # Test 1: Known Population Covariance with Power-Law Spectrum + Small-Eigenvalue Alpha
    # Construct a population where 75% of R_*^2 lives in small eigenvalues (tail) and 25% in top PCs.
    P = 300
    T = 180  # c = P/T = 1.67 > 1 (high-dimensional regime)
    idx = np.arange(1, P + 1, dtype=float)
    # Eigenvalues decay from 1.0 down to 0.002
    pop_lambdas = 0.8 * (idx ** -1.15) + 0.002

    # True squared Sharpe contributions R_i^2 = mu_i^2 / lambda_i
    # Place 25% of R_*^2 in top 20 eigenvalues and 75% in modes 21..300 (small eigenvalues)
    true_R_star_sq = 0.50  # per-period squared Sharpe (annualized SR = sqrt(12 * 0.50) = 2.449)
    r_sq_modes = np.zeros(P)
    r_sq_modes[:20] = (0.25 * true_R_star_sq) / 20.0
    r_sq_modes[20:] = (0.75 * true_R_star_sq) / (P - 20.0)
    pop_mu = np.sqrt(r_sq_modes * pop_lambdas)

    # Exact population quantities at z = 0.01
    z_test = 0.01
    pop_B0 = float(np.sum((pop_mu ** 2) / (z_test + pop_lambdas)))
    pop_B1 = float(np.sum(z_test * (pop_mu ** 2) / ((z_test + pop_lambdas) ** 2)))
    pop_H1_L = pop_B0 + pop_B1 / (1.0 - pop_B1 / pop_B0)

    # Run 100 Monte Carlo draws to verify unbiasedness of Theta_0, Theta_1, H_1^L, H_1^U, and Prop 4 OOS Ridge SR
    n_mc = 100
    mc_plug_in = []
    mc_theta0 = []
    mc_theta1 = []
    mc_H1_L = []
    mc_H1_U = []
    mc_Z_star_prime = []
    mc_ridge_oos_sr2 = []
    mc_ridge_bound = []
    mc_ho_oos_sr2 = []

    sqrt_lambdas = np.sqrt(pop_lambdas)
    for _ in range(n_mc):
        # Draw T x P Gaussian returns R_t = mu + eps_t
        Z_noise = rng.standard_normal((T, P))
        R_mat = pop_mu[None, :] + Z_noise * sqrt_lambdas[None, :]

        # Compute estimator at z_test
        R_bar = np.mean(R_mat, axis=0)
        X_c = R_mat - R_bar[None, :]
        _, s_vals, Vt = np.linalg.svd(X_c / math.sqrt(float(T)), full_matrices=False)
        pos_eigs = s_vals ** 2
        proj_pos = (Vt @ R_bar) ** 2
        rem_norm_sq = max(0.0, float(np.dot(R_bar, R_bar) - np.sum(proj_pos)))
        n_zero = P - len(pos_eigs)
        eigvals = np.concatenate([np.zeros(n_zero), pos_eigs])
        proj_zeros = np.zeros(n_zero)
        if n_zero > 0:
            proj_zeros[0] = rem_norm_sq
        proj_sq = np.concatenate([proj_zeros, proj_pos])

        res = compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=z_test)
        mc_plug_in.append(res.theta_hat)
        mc_theta0.append(res.theta_0)
        mc_theta1.append(res.theta_1)
        mc_H1_L.append(res.H1_L)
        if math.isfinite(res.H1_U):
            mc_H1_U.append(res.H1_U)
        mc_Z_star_prime.append(res.Z_star_prime)
        mc_ridge_bound.append(res.ridge_oos_sr2_bound)

        # True out-of-sample squared Sharpe of Ridge Markowitz portfolio pi_hat(z) = (zI + Sigma_hat)^{-1} R_bar
        # Using Woodbury / SVD: (zI + Sigma_hat)^{-1} R_bar = z^{-1} R_bar + V ((z + s^2)^{-1} - z^{-1}) V^T R_bar
        v_proj = Vt @ R_bar
        w_ridge = (R_bar / z_test) + Vt.T @ (((1.0 / (z_test + pos_eigs)) - (1.0 / z_test)) * v_proj)
        oos_mean = float(np.dot(w_ridge, pop_mu))
        oos_var = float(np.dot(w_ridge ** 2, pop_lambdas))
        oos_sr2 = (oos_mean ** 2) / oos_var if oos_var > 0 else 0.0
        mc_ridge_oos_sr2.append(oos_sr2)

        # True OOS squared Sharpe of 1st-order Neumann-restored portfolio:
        # w_HO(z) = (zI + Sigma_hat)^{-1} R_bar + z (zI + Sigma_hat)^{-2} R_bar
        w_ho = (
            (R_bar / z_test)
            + (z_test * R_bar / (z_test ** 2))
            + Vt.T
            @ (
                (
                    (1.0 / (z_test + pos_eigs))
                    + (z_test / ((z_test + pos_eigs) ** 2))
                    - (2.0 / z_test)
                )
                * v_proj
            )
        )
        ho_mean = float(np.dot(w_ho, pop_mu))
        ho_var = float(np.dot(w_ho ** 2, pop_lambdas))
        ho_sr2 = (ho_mean ** 2) / ho_var if ho_var > 0 else 0.0
        mc_ho_oos_sr2.append(ho_sr2)

    # Check Lemma 2 theoretical bound at z -> 0+: Z_*'(0) >= c / (c - 1) for c > 1
    c_ratio = float(P) / float(T)
    lemma2_theoretical_min = c_ratio / (c_ratio - 1.0)
    res_tiny_z = compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=1e-6)

    ann = math.sqrt(12.0)
    return {
        "P": P,
        "T": T,
        "c": c_ratio,
        "z_test": z_test,
        "true_R_star_sq": true_R_star_sq,
        "true_R_star_sr_ann": math.sqrt(true_R_star_sq) * ann,
        "pop_B0_at_z": pop_B0,
        "pop_B1_at_z": pop_B1,
        "pop_H1_L_at_z": pop_H1_L,
        "mc_mean_plug_in_theta_hat": float(np.mean(mc_plug_in)),
        "mc_mean_plug_in_sr_ann": math.sqrt(max(0.0, float(np.mean(mc_plug_in)))) * ann,
        "mc_mean_theta0": float(np.mean(mc_theta0)),
        "mc_mean_theta1": float(np.mean(mc_theta1)),
        "mc_mean_H1_L": float(np.mean(mc_H1_L)),
        "mc_mean_H1_L_sr_ann": math.sqrt(max(0.0, float(np.mean(mc_H1_L)))) * ann,
        "mc_median_H1_U_sr_ann": math.sqrt(max(0.0, float(np.median(mc_H1_U)))) * ann if mc_H1_U else float("inf"),
        "mc_mean_Z_star_prime": float(np.mean(mc_Z_star_prime)),
        "lemma2_c_over_c_minus_1": lemma2_theoretical_min,
        "empirical_Z_star_prime_at_1e_6": res_tiny_z.Z_star_prime,
        "mc_mean_ridge_oos_sr2": float(np.mean(mc_ridge_oos_sr2)),
        "mc_mean_ridge_oos_sr_ann": math.sqrt(max(0.0, float(np.mean(mc_ridge_oos_sr2)))) * ann,
        "mc_mean_prop4_bound_sr2": float(np.mean(mc_ridge_bound)),
        "mc_mean_prop4_bound_sr_ann": math.sqrt(max(0.0, float(np.mean(mc_ridge_bound)))) * ann,
        "mc_mean_neumann_ho_oos_sr_ann": math.sqrt(max(0.0, float(np.mean(mc_ho_oos_sr2)))) * ann,
    }


# ============================================================================
# 3. Part B & C: Diagnostic Audit & Counterfactual on Real nofx Data
# ============================================================================

def audit_crypto_managed_portfolios() -> Dict[str, object]:
    """Audit nofx fq_crypto 15 managed legs (from msrr_study) and 45-feature linear & ReLU RF."""
    from scripts.factor_mining import ensemble_forward_study as study
    from scripts.factor_mining import ml_multifeature_study as mls
    from scripts.factor_mining import msrr_study as ms
    from scripts.factor_mining import panel

    fm_root = ROOT / "data" / "factor_mining" / "style_neutral" / "snapshot_20261105"
    frozen, _, _ = study.load_frozen(fm_root)
    factors = frozen[ms.H]
    panels = panel.load_panels("crypto", fm_root)
    splits = panel.make_splits(panels.mask.index, ms.H, panels.windows)
    tr_start, tr_end = splits["ranges"]["train"]
    val_start, val_end = ms.VAL_RANGE

    # --- B1: 15 Managed Legs from msrr_study (7 factors x L/S + BTC market) ---
    train_legs, _ = ms.training_returns(panels, factors, tr_start, tr_end)
    val_legs, _ = ms.training_returns(panels, factors, val_start, val_end)
    fwd_legs, _ = ms.training_returns(panels, factors, study.FORWARD_START, study.day(panels.mask.index[-1]))

    ann_3d = 365.0 / 3.0
    # Paper z_grid [1e-4, 1e-3, 1e-2, 1e-1] on standardized / raw returns
    # Standardize by training std so z_grid scale matches correlation/standardized covariance (lambda_bar = 1)
    tr_std_mat, tr_sigma = ms.standardise(train_legs)
    val_std_mat = val_legs.to_numpy(dtype=float) / tr_sigma
    b1_train = estimate_hidden_sharpe_grid(tr_std_mat, annualization_factor=ann_3d)
    b1_val = estimate_hidden_sharpe_grid(val_std_mat, annualization_factor=ann_3d)

    # --- B2: 45-Feature Linear Managed Portfolios vs ReLU Random Feature Portfolios ---
    cube, feat_names, _, _ = mls.feature_panels(panels, factors, progress=lambda _: None)
    close = panels.fields["close"]
    mask_np = panels.mask.to_numpy()
    positions = study.signal_positions(close.index, tr_start, ms.H)

    d = len(feat_names)
    rng_500 = np.random.default_rng(42)
    omega_250 = rng_500.standard_normal((250, d))
    omega_500 = np.vstack([omega_250, -omega_250])

    rng_2000 = np.random.default_rng(42)
    omega_1000 = rng_2000.standard_normal((1000, d))
    omega_2000 = np.vstack([omega_1000, -omega_1000])

    lin_rets = []
    rff500_rets = []
    rff2000_rets = []
    period_dates = []

    for first, last in zip(positions[:-1], positions[1:]):
        if close.index[last] > pd.Timestamp(tr_end):
            break
        entry = close.iloc[first].to_numpy(dtype=float)
        exit_ = close.iloc[last].to_numpy(dtype=float)
        elig = mask_np[first] & np.isfinite(entry) & (entry > 0) & np.isfinite(exit_) & (exit_ > 0)
        N_t = int(np.count_nonzero(elig))
        if N_t < 10:
            continue
        r_next = (exit_[elig] / entry[elig]) - 1.0
        r_next_dm = r_next - np.mean(r_next)

        # cube[first, elig, :] is already rank-mapped in [-sqrt(3), +sqrt(3)]; rescale to [-0.5, 0.5]
        X_t = cube[first, elig, :] / math.sqrt(12.0)

        lin_rets.append((X_t.T @ r_next_dm) / float(N_t))

        scale = 0.5 * math.sqrt(2.0 / float(d))
        S_500 = np.maximum(scale * (X_t @ omega_500.T), 0.0)
        S_500_dm = S_500 - np.mean(S_500, axis=0, keepdims=True)
        rff500_rets.append((S_500_dm.T @ r_next_dm) / float(N_t))

        S_2000 = np.maximum(scale * (X_t @ omega_2000.T), 0.0)
        S_2000_dm = S_2000 - np.mean(S_2000, axis=0, keepdims=True)
        rff2000_rets.append((S_2000_dm.T @ r_next_dm) / float(N_t))
        period_dates.append(close.index[last])

    lin_mat = np.asarray(lin_rets, dtype=float)
    rff500_mat = np.asarray(rff500_rets, dtype=float)
    rff2000_mat = np.asarray(rff2000_rets, dtype=float)

    b2_lin = estimate_hidden_sharpe_grid(lin_mat, annualization_factor=ann_3d)
    b2_rff500 = estimate_hidden_sharpe_grid(rff500_mat, annualization_factor=ann_3d)
    b2_rff2000 = estimate_hidden_sharpe_grid(rff2000_mat, annualization_factor=ann_3d)

    # --- Part C: Counterfactual Walk-Forward Portfolio Check on 15 Managed Legs ---
    train_R = train_legs.to_numpy(dtype=float)
    val_R = val_legs.to_numpy(dtype=float)
    fwd_R = fwd_legs.to_numpy(dtype=float)

    counterfactual_rows = []
    w_eq = np.zeros(train_R.shape[1])
    for i, col in enumerate(train_legs.columns):
        if col.endswith("/L"):
            w_eq[i] = 1.0
        elif col.endswith("/S"):
            w_eq[i] = -1.0
    w_eq /= np.sum(np.abs(w_eq))

    def eval_weights_oos(w: np.ndarray, R_split: np.ndarray) -> Tuple[float, float]:
        port_r = R_split @ w
        mu = float(np.mean(port_r))
        sd = float(np.std(port_r, ddof=1))
        sr = (mu / sd) * math.sqrt(ann_3d) if sd > 1e-12 else 0.0
        cum = float(np.prod(1.0 + port_r) - 1.0)
        return sr, cum

    tr_sr, _ = eval_weights_oos(w_eq, train_R)
    cv_sr, cv_cum = eval_weights_oos(w_eq, val_R)
    val_sr, val_cum = eval_weights_oos(w_eq, fwd_R)
    counterfactual_rows.append({
        "method": "Baseline Equal-Weight (L-S)",
        "z": None,
        "train_sr": tr_sr,
        "cv_oos_sr": cv_sr,
        "cv_cum_ret": cv_cum,
        "val_oos_sr": val_sr,
        "val_cum_ret": val_cum,
    })

    # Standardize legs by training std (as in msrr_study)
    R_bar_tr = np.mean(tr_std_mat, axis=0)
    X_tr = tr_std_mat - R_bar_tr[None, :]
    Sigma_tr = (X_tr.T @ X_tr) / float(len(tr_std_mat))
    P_leg = tr_std_mat.shape[1]
    I_leg = np.eye(P_leg)

    for z_val in [1e-2, 1e-1, 1.0]:
        inv1 = np.linalg.inv(z_val * I_leg + Sigma_tr)
        # Standard Ridge Markowitz in standardized units, then unscale by tr_sigma
        w_r_tilde = inv1 @ R_bar_tr
        w_r = w_r_tilde / tr_sigma
        w_r /= np.sum(np.abs(w_r))
        tr_sr_r, _ = eval_weights_oos(w_r, train_R)
        cv_sr_r, cv_cum_r = eval_weights_oos(w_r, val_R)
        val_sr_r, val_cum_r = eval_weights_oos(w_r, fwd_R)
        counterfactual_rows.append({
            "method": f"Standard Ridge MSRR (z={z_val})",
            "z": z_val,
            "train_sr": tr_sr_r,
            "cv_oos_sr": cv_sr_r,
            "cv_cum_ret": cv_cum_r,
            "val_oos_sr": val_sr_r,
            "val_cum_ret": val_cum_r,
        })

        # Higher-Order Neumann-Debiased Ridge: w_HO = (zI + Sigma_hat)^{-1} R_bar + Z_*(z) (zI + Sigma_hat)^{-2} R_bar
        eig_tr, vec_tr = np.linalg.eigh(Sigma_tr)
        eig_tr = np.maximum(0.0, eig_tr)
        proj_tr = (vec_tr.T @ R_bar_tr) ** 2
        hs_tr = compute_hidden_sharpe_at_z(R_bar_tr, eig_tr, proj_tr, P=P_leg, T=len(tr_std_mat), z=z_val)
        w_ho_tilde = (inv1 + hs_tr.Z_star * (inv1 @ inv1)) @ R_bar_tr
        w_ho = w_ho_tilde / tr_sigma
        w_ho /= np.sum(np.abs(w_ho))
        tr_sr_ho, _ = eval_weights_oos(w_ho, train_R)
        cv_sr_ho, cv_cum_ho = eval_weights_oos(w_ho, val_R)
        val_sr_ho, val_cum_ho = eval_weights_oos(w_ho, fwd_R)
        counterfactual_rows.append({
            "method": f"Higher-Order Debiased Ridge (z={z_val})",
            "z": z_val,
            "train_sr": tr_sr_ho,
            "cv_oos_sr": cv_sr_ho,
            "cv_cum_ret": cv_cum_ho,
            "val_oos_sr": val_sr_ho,
            "val_cum_ret": val_cum_ho,
        })

    # PC1-Defactored Equal-Weight (projecting out top 1 sample eigenvector of Sigma_tr)
    eig_tr, vec_tr = np.linalg.eigh(Sigma_tr)
    pc1 = vec_tr[:, -1:]
    w_defac = w_eq - pc1 @ (pc1.T @ w_eq)
    w_defac /= np.sum(np.abs(w_defac))
    tr_sr_df, _ = eval_weights_oos(w_defac, train_R)
    cv_sr_df, cv_cum_df = eval_weights_oos(w_defac, val_R)
    val_sr_df, val_cum_df = eval_weights_oos(w_defac, fwd_R)
    counterfactual_rows.append({
        "method": "PC1-Defactored Equal-Weight",
        "z": None,
        "train_sr": tr_sr_df,
        "cv_oos_sr": cv_sr_df,
        "cv_cum_ret": cv_cum_df,
        "val_oos_sr": val_sr_df,
        "val_cum_ret": val_cum_df,
    })

    return {
        "b1_15legs_train": b1_train,
        "b1_15legs_cv": b1_val,
        "b2_45feat_linear_preval": b2_lin,
        "b2_45feat_rff500_preval": b2_rff500,
        "b2_45feat_rff2000_preval": b2_rff2000,
        "part_c_counterfactual": counterfactual_rows,
    }


def audit_us_equity_managed_portfolios() -> Dict[str, object]:
    """Audit nofx US Equity (bStock mega-cap ~90 stocks vs broad S&P 500 ~498 stocks)
    to test Paper Section 5.2 & 5.4 (Figure 5): does ReLU RFF add MSR over linear in mega-caps?
    """
    from scripts.factor_mining import us_ml_multifeature_study as us_mls

    broad_root = ROOT / "data" / "factor_mining" / "us_broad"
    snapshot_root = ROOT / "data" / "factor_mining" / "us_momentum" / "snapshot_20261105"
    u_pool, t_pool, _, _, _, _ = us_mls.load_pools(broad_root, snapshot_root, frozen=True)

    def build_us_managed_returns(
        pool_obj,
        start_date: str,
        end_date: str,
        rebal_step: int = 5,
    ) -> Tuple[np.ndarray, np.ndarray]:
        close_df = pool_obj.fields["close"]
        vol_df = pool_obj.fields["volume"]
        mask_df = pool_obj.mask
        ret_1d = pool_obj.fields["ret"]

        # 10 classic daily equity characteristics (momentum, reversal, vol, skew, amihud, volume trend)
        chars = {
            "mom_5": close_df.pct_change(5, fill_method=None),
            "mom_21": close_df.pct_change(21, fill_method=None),
            "mom_63": close_df.pct_change(63, fill_method=None),
            "mom_126": close_df.pct_change(126, fill_method=None),
            "mom_252": close_df.pct_change(252, fill_method=None),
            "vol_21": ret_1d.rolling(21, min_periods=15).std(),
            "vol_63": ret_1d.rolling(63, min_periods=30).std(),
            "skew_21": ret_1d.rolling(21, min_periods=15).skew(),
            "amihud_21": (ret_1d.abs() / (close_df * vol_df + 1.0)).rolling(21, min_periods=15).mean(),
            "vol_chg_21": vol_df / (vol_df.rolling(21, min_periods=15).mean() + 1.0),
        }
        c_names = sorted(chars.keys())
        d_c = len(c_names)

        pre_mask = (close_df.index >= pd.Timestamp(start_date)) & (close_df.index <= pd.Timestamp(end_date))
        sub_dates = close_df.index[pre_mask]
        idxs = list(range(0, len(sub_dates) - rebal_step, rebal_step))

        rng = np.random.default_rng(42)
        omega_250 = rng.standard_normal((250, d_c))
        omega_500 = np.vstack([omega_250, -omega_250])

        lin_list = []
        rff_list = []
        for idx in idxs:
            dt = sub_dates[idx]
            dt_next = sub_dates[idx + rebal_step]
            c_now = close_df.loc[dt]
            c_nxt = close_df.loc[dt_next]
            elig = mask_df.loc[dt] & c_now.notna() & c_nxt.notna() & (c_now > 0) & (c_nxt > 0)
            if elig.sum() < 15:
                continue
            syms = elig[elig].index
            N_t = len(syms)
            r_fwd = ((c_nxt[syms] / c_now[syms]) - 1.0).to_numpy(dtype=float)
            r_fwd_dm = r_fwd - np.mean(r_fwd)

            X_t = np.zeros((N_t, d_c), dtype=float)
            for j, cn in enumerate(c_names):
                s = chars[cn].loc[dt, syms]
                rk = s.rank(pct=True, na_option="keep") - 0.5
                X_t[:, j] = rk.fillna(0.0).to_numpy(dtype=float)

            lin_list.append((X_t.T @ r_fwd_dm) / float(N_t))
            S_t = np.maximum(0.5 * math.sqrt(2.0 / float(d_c)) * (X_t @ omega_500.T), 0.0)
            S_t_dm = S_t - np.mean(S_t, axis=0, keepdims=True)
            rff_list.append((S_t_dm.T @ r_fwd_dm) / float(N_t))

        return np.asarray(lin_list, dtype=float), np.asarray(rff_list, dtype=float)

    ann_5d = 252.0 / 5.0
    # bStock snapshot starts in late 2020 (warmed up by late 2021); use 2022-01-01..2024-12-31 (~150 5d periods)
    bstock_lin, bstock_rff500 = build_us_managed_returns(t_pool, "2022-01-01", "2024-12-31", rebal_step=5)
    sp500_lin, sp500_rff500 = build_us_managed_returns(u_pool, "2012-11-01", "2021-12-31", rebal_step=5)

    return {
        "bstock_mega_linear_10": estimate_hidden_sharpe_grid(bstock_lin, annualization_factor=ann_5d),
        "bstock_mega_rff500": estimate_hidden_sharpe_grid(bstock_rff500, annualization_factor=ann_5d),
        "sp500_broad_linear_10": estimate_hidden_sharpe_grid(sp500_lin, annualization_factor=ann_5d),
        "sp500_broad_rff500": estimate_hidden_sharpe_grid(sp500_rff500, annualization_factor=ann_5d),
    }


def main() -> None:
    print("=" * 80)
    print("SSRN 7679420 ('The Hidden Sharpe Ratio') — Verification & nofx Diagnostic")
    print("=" * 80)

    print("\n[Part A] Running synthetic DGP & Proposition 1 / Lemma 2 / Proposition 4 verification...")
    part_a = run_part_a_synthetic_verification()
    print(f"  Population True R_* (ann SR):           {part_a['true_R_star_sr_ann']:.3f} (R_*^2 = {part_a['true_R_star_sq']:.3f})")
    print(f"  Naive Plug-in GRS sqrt(Theta_hat(z)):   {part_a['mc_mean_plug_in_sr_ann']:.3f} (Theta_hat = {part_a['mc_mean_plug_in_theta_hat']:.3f} vs pop B0 = {part_a['pop_B0_at_z']:.3f})")
    print(f"  Debiased 0th-order Theta_0 vs Pop B0:   {part_a['mc_mean_theta0']:.4f} vs {part_a['pop_B0_at_z']:.4f}")
    print(f"  Debiased 1st-order Theta_1 vs Pop B1:   {part_a['mc_mean_theta1']:.4f} vs {part_a['pop_B1_at_z']:.4f}")
    print(f"  Debiased Lower Bound sqrt(H_1^L) (ann): {part_a['mc_mean_H1_L_sr_ann']:.3f} (H_1^L = {part_a['mc_mean_H1_L']:.4f} vs pop H_1^L = {part_a['pop_H1_L_at_z']:.4f})")
    print(f"  Debiased Upper Bound sqrt(H_1^U) (ann): {part_a['mc_median_H1_U_sr_ann']:.3f}")
    print(f"  Lemma 2 Z_*'(1e-6) vs c/(c-1):          {part_a['empirical_Z_star_prime_at_1e_6']:.3f} >= {part_a['lemma2_c_over_c_minus_1']:.3f}")
    print(f"  Prop 4 Ridge OOS SR vs Ceiling:         {part_a['mc_mean_ridge_oos_sr_ann']:.3f} <= {part_a['mc_mean_prop4_bound_sr_ann']:.3f} (Z_*'(z) = {part_a['mc_mean_Z_star_prime']:.3f})")
    print(f"  Neumann 1st-Order Restored OOS SR:      {part_a['mc_mean_neumann_ho_oos_sr_ann']:.3f}")

    print("\n[Part B1/B2 & Part C] Auditing nofx fq_crypto Managed Portfolios...")
    crypto_res = audit_crypto_managed_portfolios()

    def _print_summary(label: str, d: Dict[str, object]) -> None:
        g01 = next((g for g in d["grid"] if abs(g["z"] - 0.01) < 1e-9), d["grid"][-1])
        ann = math.sqrt(float(d["annualization_factor"]))
        plug_sr = math.sqrt(max(0.0, g01["theta_hat"])) * ann
        th0_sr = math.sqrt(max(0.0, g01["theta_0"])) * ann
        th1_sr = math.sqrt(max(0.0, g01["theta_1"])) * ann
        print(
            f"  {label:<32} | T={d['T']:<3} P={d['P']:<4} c={d['c']:.2f} | "
            f"Plug-in(z=.01)={plug_sr:6.2f} | Theta0={th0_sr:5.2f} (SE={g01['se_theta0']*ann**2:.2f}) | "
            f"Theta1={th1_sr:5.2f} | H1_L={d['combined_H1_L_sr_ann']:5.2f} | "
            f"Z_*'(.01)={g01['Z_star_prime']:6.2f} | RidgeOOS_Ceil={d['ridge_oos_sr_ceiling_ann']:5.2f}"
        )

    _print_summary("Crypto 15-Leg (Train)", crypto_res["b1_15legs_train"])
    _print_summary("Crypto 15-Leg (CV)", crypto_res["b1_15legs_cv"])
    _print_summary("Crypto 45-Feat Linear (PreVal)", crypto_res["b2_45feat_linear_preval"])
    _print_summary("Crypto 45-Feat RFF-500 (PreVal)", crypto_res["b2_45feat_rff500_preval"])
    _print_summary("Crypto 45-Feat RFF-2000 (PreVal)", crypto_res["b2_45feat_rff2000_preval"])

    print("\n[Part C] Crypto 15-Leg Counterfactual Portfolio Walk-Forward Check:")
    for row in crypto_res["part_c_counterfactual"]:
        print(
            f"  {row['method']:<36} | Train SR={row['train_sr']:+6.2f} | "
            f"CV OOS SR={row['cv_oos_sr']:+6.2f} (Cum={row['cv_cum_ret']*100:+6.1f}%) | "
            f"Val OOS SR={row['val_oos_sr']:+6.2f} (Cum={row['val_cum_ret']*100:+6.1f}%)"
        )

    print("\n[Part B3] Auditing nofx fq_us (bStock Mega-Cap vs S&P 500 Broad) Managed Portfolios...")
    us_res = audit_us_equity_managed_portfolios()
    _print_summary("US bStock Mega-Cap Linear-10", us_res["bstock_mega_linear_10"])
    _print_summary("US bStock Mega-Cap RFF-500", us_res["bstock_mega_rff500"])
    _print_summary("US S&P500 Broad Linear-10", us_res["sp500_broad_linear_10"])
    _print_summary("US S&P500 Broad RFF-500", us_res["sp500_broad_rff500"])

    out_path = Path("/Users/zhangyun/Library/Application Support/orca/antigravity-chat/antigravity-acp/brain/75ed1744-ad87-4cc0-a88c-8a63342e3841/scratch/hidden_sharpe_results.json")
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(
            {
                "part_a_synthetic": part_a,
                "crypto": crypto_res,
                "us_equity": us_res,
            },
            f,
            indent=2,
            default=str,
        )
    print(f"\nSaved full JSON diagnostics to {out_path}")


if __name__ == "__main__":
    main()
