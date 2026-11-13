"""Numeric check of the closed forms in arXiv:2608.00885 (Amaral 2026,
"Optimal Trading of Microstructure Mean Reversion"), surrogate level only.

No market data, no returns, no IC: units are s_G = 1 and alpha = 1, and the
only input is the spread-to-dispersion ratio gamma = phi / s_G.

Checks
  1. band equation root u* = (gamma + sqrt(gamma^2 + 4)) / 2      (paper eq. 3.7)
     vs the exact Dawson maximiser u_D of the surrogate rate        (eq. B.1)
  2. R(u_D) equals sqrt(2/pi) * exp(-u_D^2 / 2)                     (eq. 3.8 / Prop. B.2)
  3. the paper's claims: for gamma >= 0.4 threshold gap <= ~6 %, rate loss <= 2.3 %
     (near gamma ~ 1.7); ~9 % rate loss at gamma = 0.05; myopic band u = gamma earns 0
  4. a Monte Carlo of the Gaussian surrogate under the band, gamma = 0.5
  5. the rate when a taker fee is added to phi (paper p.9: "any per-lot fee simply
     adds to it"), for gamma in {5, 10, 50}

Run: .venv/bin/python -I scripts/backtest/ou_band_formula_check.py
"""

import numpy as np


def _int_exp_half_sq(u: float) -> float:
    """int_0^u exp(x^2 / 2) dx (trapezoid; u <= 12 keeps it finite)."""
    x = np.linspace(0.0, u, 20001)
    return float(np.trapezoid(np.exp(x * x / 2), x))


def rate(u: float, g: float) -> float:
    """Surrogate rate R(u) / (alpha s_G) = sqrt(2/pi) (u - g) / int_0^u e^{x^2/2} dx.

    Equivalent to (2/pi) (u - g) / erfi(u / sqrt 2): eq. (3.5) with Prop. B.1.
    """
    return float(np.sqrt(2 / np.pi) * (u - g) / _int_exp_half_sq(u))


def _foc(u: float, g: float) -> float:
    # dR/du = 0  <=>  int_0^u e^{x^2/2} dx = (u - g) e^{u^2/2}  <=>  eq. (B.1)
    return _int_exp_half_sq(u) - (u - g) * np.exp(u * u / 2)


def _bisect(f, a: float, b: float) -> float:
    for _ in range(200):
        m = (a + b) / 2
        if f(a) * f(m) <= 0:
            b = m
        else:
            a = m
    return (a + b) / 2


def u_dawson(g: float) -> float:
    return _bisect(lambda u: _foc(u, g), g + 1e-9, 12.0)


def u_star(g: float) -> float:
    return 0.5 * (g + np.sqrt(g * g + 4))


def main() -> None:
    print("gamma    uD      u*   thr_diff%  rate_loss%   R(uD)   sqrt(2/pi)e^-uD^2/2")
    for g in [0.05, 0.1, 0.28, 0.4, 0.5, 1.0, 1.7, 3.0, 5.0, 10.0]:
        a, b = u_dawson(g), u_star(g)
        r_d, r_s = rate(a, g), rate(b, g)
        print(
            f"{g:5.2f} {a:7.3f} {b:7.3f} {100 * abs(b - a) / a:9.2f} "
            f"{100 * (1 - r_s / r_d):10.2f}   {r_d:.3e}   {np.sqrt(2 / np.pi) * np.exp(-a * a / 2):.3e}"
        )
    gs = np.linspace(0.4, 3.0, 131)
    loss = [100 * (1 - rate(u_star(g), g) / rate(u_dawson(g), g)) for g in gs]
    gap = [100 * abs(u_star(g) - u_dawson(g)) / u_dawson(g) for g in gs]
    print(
        "gamma>=0.4: max rate loss %.2f%% at gamma=%.2f; max threshold gap %.2f%%"
        % (max(loss), gs[int(np.argmax(loss))], max(gap))
    )
    print("myopic band (u = gamma = 0.5) rate:", rate(0.5, 0.5))
    # gamma = 50: R ~ sqrt(2/pi) exp(-u*^2/2) with u* ~ 50.02 (closed form; no quadrature)
    us = u_star(50.0)
    print("gamma=50 closed-form rate: 10^%.0f" % (np.log10(np.sqrt(2 / np.pi)) - us * us / 2 / np.log(10)))

    # Monte Carlo of the surrogate dG = -G dt + sqrt(2) dW (s_G = 1), exact OU steps.
    rng = np.random.default_rng(0)
    g = 0.5
    u = u_dawson(g)
    dt, n = 2e-4, 10_000_000
    a = np.exp(-dt)
    z = rng.standard_normal(n) * np.sqrt(1 - a * a)
    G, q, pnl = 0.0, 0, 0.0
    for i in range(n):
        G = a * G + z[i]
        if q != 1 and G <= -u:  # buy (1 lot first entry, 2 lots on a flip); X-marked cash
            pnl += (1 - q) * (-G - g)
            q = 1
        elif q != -1 and G >= u:
            pnl += (q + 1) * (G - g)
            q = -1
    T = n * dt
    print("MC T=%d rate %.4f vs formula %.4f" % (T, pnl / T, rate(u, g)))


if __name__ == "__main__":
    main()
