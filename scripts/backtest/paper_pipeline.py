"""Automated Quantitative Paper Indexing, Dedup, Extraction & Queue Ledger CLI.

Part of the nofx long-term paper research pipeline
(``docs/architecture/PAPER_RESEARCH_PIPELINE_DESIGN_2026-11-18.md``).

Subcommands:
    index    Scan NAS curated folders + q-fin-archive/ssrn-fen catalog CSVs,
             deduplicate against researchLog.ts & docs/research, score relevance.
    next     Show top N unstudied candidate papers with priority track & paths.
    extract  Safely extract PDF text via pdftotext to local queue cache.
    record   Append paper lifecycle status to data/research/paper_queue/ledger.jsonl
             with circuit-breaker checks (consecutive error streak / K budget).

Never modifies live trading DB, kernel/, store/, or scripts/factor_mining/evaluate.py.
"""
from __future__ import annotations

import argparse
import csv
from dataclasses import asdict, dataclass
import datetime as dt
import fcntl
import json
from pathlib import Path
import re
import shutil
import subprocess
from typing import Any


REPO = Path(__file__).resolve().parents[2]
DEFAULT_PAPERS_ROOT = Path("/Volumes/homes/liz/nofx_data/papers")
DEFAULT_QUEUE_DIR = REPO / "data/research/paper_queue"
RESEARCH_LOG_TS = REPO / "web/src/constants/researchLog.ts"
DOCS_RESEARCH_DIR = REPO / "docs/research"
EVOLUTION_LEDGER = REPO / "data/evolution/ledger.jsonl"

CURATED_SUBDIRS = (
    "01-因子挖掘",
    "02-交易执行与成本",
    "03-量化系统",
    "04-LLM交易",
    "05-研究可靠性",
    "06-加密市场",
)

TOPIC_RULES: list[tuple[str, float, tuple[str, ...]]] = [
    ("crypto_perps_factors", 3.5, ("crypto", "cryptocurrency", "perpetual", "funding rate", "bitcoin", "ethereum", "altcoin", "binance")),
    ("llm_trading_audit", 3.0, ("large language model", "llm", "agent", "multi-agent", "trading agent", "calibration", "overconfidence", "token cost")),
    ("overfitting_validation", 2.8, ("deflated sharpe", "backtest overfitting", "multiple testing", "false discovery", "pbo", "selection bias", "data snooping", "minimum track record")),
    ("factor_mining_ml", 2.5, ("alpha", "factor mining", "asset pricing", "momentum", "cross-section", "factor zoo", "formulaic alpha", "symbolic", "mcts", "reinforcement learning")),
    ("execution_microstructure", 2.4, ("optimal execution", "limit order", "slippage", "market impact", "hedging", "transaction cost", "microstructure", "stop loss", "liquidity")),
]

DOWNWEIGHT_TERMS = (
    "option pricing",
    "black-scholes",
    "stochastic volatility",
    "credit default",
    "yield curve",
    "mortgage",
    "insurance",
    "macroeconomic dsge",
    "corporate governance",
    "bankruptcy law",
    "merger arbitrage",
)

TRACK_HINTS = {
    "crypto_perps_factors": "fq_crypto / crypto_carry",
    "llm_trading_audit": "crypto_llm / us_llm",
    "overfitting_validation": "S0/S1 methodology (non-K)",
    "factor_mining_ml": "fq_crypto / fq_us",
    "execution_microstructure": "execution_rule (B-type)",
}


@dataclass
class PaperCandidate:
    paper_id: str
    tier: str  # curated | q-fin | ssrn
    title: str
    authors: str
    category: str
    pdf_path: str
    source_meta: str
    relevance_score: float
    matched_topics: list[str]
    suggested_track: str
    downloads: int
    status: str  # pending | done | extracted | ...


def load_studied_identifiers(
    research_log_ts: Path = RESEARCH_LOG_TS,
    docs_dir: Path = DOCS_RESEARCH_DIR,
    queue_ledger: Path | None = None,
) -> tuple[set[str], set[str]]:
    """Extract known paper IDs (arXiv, SSRN, NBER) and normalized title tokens from repo records."""
    ids: set[str] = set()
    corpus_parts: list[str] = []
    if research_log_ts.exists():
        corpus_parts.append(research_log_ts.read_text(encoding="utf-8", errors="ignore"))
    if docs_dir.exists():
        for md in sorted(docs_dir.glob("*.md")):
            corpus_parts.append(md.name)
            corpus_parts.append(md.read_text(encoding="utf-8", errors="ignore")[:4000])
    corpus = "\n".join(corpus_parts)

    for m in re.findall(r"\b\d{4}\.\d{4,5}\b", corpus):
        ids.add(m)
    for m in re.findall(r"(?i)(?:ssrn[-:\s]*)(\d{6,8})", corpus):
        ids.add(f"ssrn-{m}")
        ids.add(m)
    for m in re.findall(r"(?i)(w\d{4,6})", corpus):
        ids.add(m.lower())

    # Also read queue ledger terminal statuses
    ledger_done: set[str] = set()
    if queue_ledger and queue_ledger.exists():
        for line in queue_ledger.read_text(encoding="utf-8", errors="ignore").splitlines():
            if not line.strip():
                continue
            try:
                row = json.loads(line)
            except ValueError:
                continue
            pid = str(row.get("paper_id") or "")
            if row.get("status") in ("assessed", "verified", "skipped", "blocked_missing_data", "done"):
                ledger_done.add(pid)
                ids.add(pid)
    return ids, ledger_done


def score_paper_text(title: str, category: str = "", downloads: int = 0, tier: str = "q-fin") -> tuple[float, list[str], str]:
    """Deterministic relevance score for nofx priorities."""
    hay = f"{title} {category}".lower()
    score = 0.0
    topics: list[str] = []
    best_topic = ""
    best_w = -1.0
    for topic, weight, kws in TOPIC_RULES:
        hits = sum(1 for kw in kws if kw in hay)
        if hits > 0:
            topics.append(topic)
            score += weight + min(hits - 1, 3) * 0.6
            if weight > best_w:
                best_w = weight
                best_topic = topic

    for bad in DOWNWEIGHT_TERMS:
        if bad in hay:
            score -= 2.0

    if tier == "curated":
        score += 5.0
    if downloads > 0:
        # Smooth log popularity bonus up to +3.0
        import math
        score += min(3.0, math.log10(downloads + 1) * 0.55)
    if category.lower() in ("q-fin.tr", "q-fin.pm", "q-fin.st", "q-fin.cp"):
        score += 1.0

    suggested = TRACK_HINTS.get(best_topic, "general_research")
    return round(score, 3), topics, suggested


def _is_studied(paper_id: str, title: str, studied_ids: set[str], full_corpus_lower: str) -> bool:
    if paper_id in studied_ids:
        return True
    norm_id = paper_id.lower()
    if norm_id in studied_ids:
        return True
    # Match curated well-known filenames studied previously
    low_t = re.sub(r"[-_\s]+", "", title.lower())
    corpus_compact = re.sub(r"[-_\s]+", "", full_corpus_lower)
    known_markers = (
        "alphaillusion",
        "deflatedsharperatio",
        "commonriskfactors",
        "hiddensharpe",
    )
    for marker in known_markers:
        if marker in low_t and marker in corpus_compact:
            return True
    return False


def build_index(
    papers_root: Path = DEFAULT_PAPERS_ROOT,
    queue_dir: Path = DEFAULT_QUEUE_DIR,
    *,
    research_log_ts: Path = RESEARCH_LOG_TS,
    docs_dir: Path = DOCS_RESEARCH_DIR,
    max_archive_entries: int = 500,
) -> dict[str, Any]:
    """Scan curated folders + q-fin-archive/ssrn-fen catalog.csv without SMB directory globbing."""
    if not papers_root.exists():
        raise FileNotFoundError(f"Papers root unavailable (NAS unmounted?): {papers_root}")

    queue_ledger = queue_dir / "ledger.jsonl"
    studied_ids, ledger_done = load_studied_identifiers(research_log_ts, docs_dir, queue_ledger)
    corpus_lower = ""
    if research_log_ts.exists():
        corpus_lower += research_log_ts.read_text(encoding="utf-8", errors="ignore").lower()
    if docs_dir.exists():
        corpus_lower += "\n" + "\n".join(p.name.lower() for p in docs_dir.glob("*.md"))

    candidates: list[PaperCandidate] = []

    # 1. Curated folders (small, safe to list)
    curated_count = 0
    for sub in CURATED_SUBDIRS:
        sub_dir = papers_root / sub
        if not sub_dir.is_dir():
            continue
        for pdf in sorted(sub_dir.iterdir()):
            if pdf.suffix.lower() != ".pdf":
                continue
            curated_count += 1
            stem = pdf.stem
            # Extract arXiv / NBER / canonical id if present
            m_arxiv = re.search(r"arXiv[-_]?(\d{4}\.\d{4,5})", stem, re.I)
            m_nber = re.search(r"NBER[-_]?(w\d{4,6})", stem, re.I)
            pid = f"arxiv:{m_arxiv.group(1)}" if m_arxiv else (f"nber:{m_nber.group(1).lower()}" if m_nber else f"curated:{stem}")
            raw_tokens = [pid, m_arxiv.group(1) if m_arxiv else "", m_nber.group(1).lower() if m_nber else ""]
            already = any(t and t in studied_ids for t in raw_tokens) or _is_studied(pid, stem, studied_ids, corpus_lower)
            sc, topics, track = score_paper_text(f"{stem} {sub}", category=sub, tier="curated")
            candidates.append(
                PaperCandidate(
                    paper_id=pid,
                    tier="curated",
                    title=stem,
                    authors="",
                    category=sub,
                    pdf_path=str(pdf),
                    source_meta=sub,
                    relevance_score=sc,
                    matched_topics=topics,
                    suggested_track=track,
                    downloads=0,
                    status="done" if already else "pending",
                )
            )

    # 2. q-fin-archive/catalog.csv (streaming CSV, zero dir glob)
    qfin_csv = papers_root / "q-fin-archive/catalog.csv"
    qfin_total = 0
    qfin_downloaded = 0
    qfin_scored: list[PaperCandidate] = []
    if qfin_csv.exists():
        with qfin_csv.open("r", encoding="utf-8", errors="replace", newline="") as f:
            reader = csv.DictReader(f)
            for row in reader:
                qfin_total += 1
                if (row.get("status") or "").strip() != "downloaded":
                    continue
                rel_pdf = (row.get("pdf_file") or "").strip()
                if not rel_pdf:
                    continue
                qfin_downloaded += 1
                aid = (row.get("id") or "").strip()
                pid = f"arxiv:{aid}"
                title = (row.get("title") or "").strip()
                cat = (row.get("primary_category") or "").strip()
                sc, topics, track = score_paper_text(title, category=cat, tier="q-fin")
                if not topics and sc < 1.0:
                    continue
                already = aid in studied_ids or _is_studied(pid, title, studied_ids, corpus_lower)
                qfin_scored.append(
                    PaperCandidate(
                        paper_id=pid,
                        tier="q-fin",
                        title=title,
                        authors=(row.get("authors") or "").strip(),
                        category=cat,
                        pdf_path=str(papers_root / "q-fin-archive" / rel_pdf),
                        source_meta=row.get("published") or "",
                        relevance_score=sc,
                        matched_topics=topics,
                        suggested_track=track,
                        downloads=0,
                        status="done" if already else "pending",
                    )
                )
    qfin_scored.sort(key=lambda c: (-c.relevance_score, c.paper_id))
    candidates.extend(qfin_scored[:max_archive_entries])

    # 3. ssrn-fen/catalog.csv
    ssrn_csv = papers_root / "ssrn-fen/catalog.csv"
    ssrn_total = 0
    ssrn_downloaded = 0
    ssrn_scored: list[PaperCandidate] = []
    if ssrn_csv.exists():
        with ssrn_csv.open("r", encoding="utf-8", errors="replace", newline="") as f:
            reader = csv.DictReader(f)
            for row in reader:
                ssrn_total += 1
                rel_pdf = (row.get("pdf_file") or "").strip()
                if not rel_pdf:
                    continue
                ssrn_downloaded += 1
                if (row.get("is_paid") or "").strip().lower() == "true":
                    continue
                sid = (row.get("id") or "").strip()
                pid = f"ssrn:{sid}"
                title = (row.get("title") or "").strip()
                try:
                    dl = int(float(row.get("downloads") or 0))
                except ValueError:
                    dl = 0
                sc, topics, track = score_paper_text(title, category="", downloads=dl, tier="ssrn")
                if not topics:
                    continue
                already = sid in studied_ids or f"ssrn-{sid}" in studied_ids or _is_studied(pid, title, studied_ids, corpus_lower)
                ssrn_scored.append(
                    PaperCandidate(
                        paper_id=pid,
                        tier="ssrn",
                        title=title,
                        authors=(row.get("authors") or "").strip(),
                        category=(row.get("publication_status") or "").strip(),
                        pdf_path=str(papers_root / "ssrn-fen" / rel_pdf),
                        source_meta=row.get("approved_date") or "",
                        relevance_score=sc,
                        matched_topics=topics,
                        suggested_track=track,
                        downloads=dl,
                        status="done" if already else "pending",
                    )
                )
    ssrn_scored.sort(key=lambda c: (-c.relevance_score, -c.downloads, c.paper_id))
    candidates.extend(ssrn_scored[:max_archive_entries])

    candidates.sort(key=lambda c: (0 if c.tier == "curated" else (1 if c.tier == "q-fin" else 2), -c.relevance_score, -c.downloads, c.paper_id))
    deduped: list[PaperCandidate] = []
    seen_pids: set[str] = set()
    for c in candidates:
        if c.paper_id in seen_pids:
            continue
        seen_pids.add(c.paper_id)
        deduped.append(c)
    candidates = deduped
    candidates.sort(key=lambda c: (0 if c.status == "pending" else 1, -c.relevance_score, -c.downloads, c.paper_id))

    queue_dir.mkdir(parents=True, exist_ok=True)
    ensure_support_files(queue_dir)
    out_doc = {
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "papers_root": str(papers_root),
        "counts": {
            "curated_pdfs": curated_count,
            "qfin_rows": qfin_total,
            "qfin_downloaded": qfin_downloaded,
            "ssrn_rows": ssrn_total,
            "ssrn_downloaded": ssrn_downloaded,
            "indexed_candidates": len(candidates),
            "pending_candidates": sum(1 for c in candidates if c.status == "pending"),
            "done_candidates": sum(1 for c in candidates if c.status == "done"),
        },
        "candidates": [asdict(c) for c in candidates],
    }
    (queue_dir / "catalog_index.json").write_text(json.dumps(out_doc, indent=2, ensure_ascii=False), encoding="utf-8")
    return out_doc


def ensure_support_files(queue_dir: Path = DEFAULT_QUEUE_DIR) -> None:
    queue_dir.mkdir(parents=True, exist_ok=True)
    lessons = queue_dir / "LESSONS.md"
    if not lessons.exists():
        lessons.write_text(
            "# Paper Research Pipeline — Shared Lessons\n\n"
            "1. **NAS MariaDB SQL Rule**: Primary key on `crypto_klines` is `(market, symbol, interval, open_time)`. "
            "Always query one symbol at a time via `nas_klines.fetch_crypto_bars(..., client=cli)` instead of large `IN (...)` + `ORDER BY symbol` scans.\n"
            "2. **SMB Directory Rule**: Never `ls`/`glob` inside `q-fin-archive/` or `ssrn-fen/` (tens of thousands of files); always stream `catalog.csv`.\n"
            "3. **Backtest Engine Rule**: All new portfolio/signal backtests MUST go through `scripts/backtest/nt_engine.py:run_portfolio` "
            "(note return dict exposes `bt['equity']`, not `bt['returns']`).\n"
            "4. **Pre-registration Rule**: Freeze `docs/research/YYYY-MM-DD-<topic>-prereg.md` + `PREREG_SHA256` BEFORE computing any returns or IC.\n"
            "5. **Honesty on `val`/`holdout`**: Existing crypto `val`/`holdout` windows have been read by `K=1..15`; offline `screen_pass` always requires live S2 pair validation.\n",
            encoding="utf-8",
        )
    human = queue_dir / "NEEDS-HUMAN.md"
    if not human.exists():
        human.write_text(
            "# Escalations Requiring Human Decision\n\n"
            "Items appended here when:\n"
            "- A strategy hypothesis passes S1 (`screen_pass` or `strong`) and is eligible for S2 (`scripts/promotion/create_pair.py`).\n"
            "- A paper requires external data unavailable in NAS MariaDB (`crypto_klines` / `us_daily`).\n"
            "- Circuit breaker trips (`>= 3` consecutive identical pipeline failures or `K` budget guard).\n\n",
            encoding="utf-8",
        )
    ledger = queue_dir / "ledger.jsonl"
    if not ledger.exists():
        ledger.write_text("", encoding="utf-8")


def _load_ledger_state(queue_dir: Path) -> tuple[dict[str, str], str | None]:
    ledger_path = queue_dir / "ledger.jsonl"
    latest_status: dict[str, str] = {}
    halt_reason: str | None = None
    if not ledger_path.exists():
        return latest_status, halt_reason
    for line in ledger_path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        pid = str(row.get("paper_id") or "")
        st = str(row.get("status") or "")
        if pid and st:
            latest_status[pid] = st
        if st == "breaker_reset":
            halt_reason = None
        elif int(row.get("same_error_streak") or 0) >= 3 or row.get("breaker_tripped"):
            halt_reason = f"same_error_streak={row.get('same_error_streak')} ({row.get('error_sig')})"
    return latest_status, halt_reason


def next_candidates(queue_dir: Path = DEFAULT_QUEUE_DIR, *, tier: str = "all", limit: int = 5) -> list[dict[str, Any]]:
    idx_path = queue_dir / "catalog_index.json"
    if not idx_path.exists():
        raise FileNotFoundError(f"Index not found; run `paper_pipeline index` first ({idx_path})")
    latest_status, halt_reason = _load_ledger_state(queue_dir)
    if halt_reason is not None:
        raise RuntimeError(f"Paper pipeline halted by circuit breaker: {halt_reason}")
    doc = json.loads(idx_path.read_text(encoding="utf-8"))
    out = []
    for c in doc.get("candidates", []):
        eff_status = latest_status.get(str(c.get("paper_id") or ""), c.get("status"))
        if eff_status != "pending":
            continue
        if tier != "all" and c.get("tier") != tier:
            continue
        out.append(c)
        if len(out) >= limit:
            break
    return out


def extract_pdf_text(paper_id: str, queue_dir: Path = DEFAULT_QUEUE_DIR) -> dict[str, Any]:
    idx_path = queue_dir / "catalog_index.json"
    doc = json.loads(idx_path.read_text(encoding="utf-8"))
    match = next((c for c in doc.get("candidates", []) if c.get("paper_id") == paper_id), None)
    if match is None:
        raise KeyError(f"Paper ID not found in index: {paper_id}")
    pdf_path = Path(match["pdf_path"])
    if not pdf_path.exists():
        raise FileNotFoundError(f"PDF file missing: {pdf_path}")
    pdftotext = shutil.which("pdftotext") or "/opt/homebrew/bin/pdftotext"
    if not Path(pdftotext).exists():
        raise RuntimeError("pdftotext binary not found")

    safe_name = re.sub(r"[^A-Za-z0-9._-]+", "_", paper_id)
    out_dir = queue_dir / "extracted"
    out_dir.mkdir(parents=True, exist_ok=True)
    out_txt = out_dir / f"{safe_name}.txt"
    subprocess.run([pdftotext, "-layout", str(pdf_path), str(out_txt)], check=True, timeout=60)
    text = out_txt.read_text(encoding="utf-8", errors="replace")
    return {
        "paper_id": paper_id,
        "pdf_path": str(pdf_path),
        "text_path": str(out_txt),
        "lines": len(text.splitlines()),
        "chars": len(text),
    }


def record_status(
    paper_id: str,
    status: str,
    *,
    notes: str = "",
    error_sig: str = "",
    verdict: str = "",
    queue_dir: Path = DEFAULT_QUEUE_DIR,
) -> dict[str, Any]:
    ensure_support_files(queue_dir)
    ledger_path = queue_dir / "ledger.jsonl"
    lock_path = queue_dir / ".ledger.lock"

    with lock_path.open("a+", encoding="utf-8") as lf:
        fcntl.flock(lf.fileno(), fcntl.LOCK_EX)
        try:
            prev_rows = [json.loads(l) for l in ledger_path.read_text(encoding="utf-8").splitlines() if l.strip()]

            # Circuit breaker: count trailing consecutive identical errors
            streak = 0
            if status == "error" and error_sig:
                streak = 1
                for r in reversed(prev_rows):
                    if r.get("status") == "error" and r.get("error_sig") == error_sig:
                        streak += 1
                    else:
                        break

            breaker_tripped = bool(streak >= 3)
            row = {
                "recorded_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                "paper_id": paper_id,
                "status": status,
                "verdict": verdict,
                "error_sig": error_sig,
                "same_error_streak": streak,
                "breaker_tripped": breaker_tripped,
                "notes": notes,
            }
            with ledger_path.open("a", encoding="utf-8") as f:
                f.write(json.dumps(row, ensure_ascii=False) + "\n")

            if breaker_tripped or verdict in ("screen_pass", "strong") or status == "blocked_missing_data":
                human_path = queue_dir / "NEEDS-HUMAN.md"
                reason = (
                    f"Circuit breaker: same error `{error_sig}` occurred {streak}x in a row"
                    if breaker_tripped
                    else (f"S1 verdict `{verdict}` eligible for S2 review" if verdict in ("screen_pass", "strong") else f"Blocked: {notes}")
                )
                with human_path.open("a", encoding="utf-8") as hf:
                    hf.write(f"- [{row['recorded_at'][:10]}] `{paper_id}` — {reason}. Notes: {notes}\n")
                row["escalated_human"] = True
            else:
                row["escalated_human"] = False
        finally:
            fcntl.flock(lf.fileno(), fcntl.LOCK_UN)
    return row


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_idx = sub.add_parser("index", help="Build deduplicated paper catalog index from NAS")
    p_idx.add_argument("--papers-root", type=Path, default=DEFAULT_PAPERS_ROOT)
    p_idx.add_argument("--queue-dir", type=Path, default=DEFAULT_QUEUE_DIR)
    p_idx.add_argument("--max-archive", type=int, default=500)

    p_nxt = sub.add_parser("next", help="List top N pending papers")
    p_nxt.add_argument("--queue-dir", type=Path, default=DEFAULT_QUEUE_DIR)
    p_nxt.add_argument("--tier", choices=("all", "curated", "q-fin", "ssrn"), default="all")
    p_nxt.add_argument("--limit", type=int, default=5)

    p_ext = sub.add_parser("extract", help="Extract PDF text for a paper_id")
    p_ext.add_argument("--paper-id", required=True)
    p_ext.add_argument("--queue-dir", type=Path, default=DEFAULT_QUEUE_DIR)

    p_rec = sub.add_parser("record", help="Record lifecycle status into queue ledger")
    p_rec.add_argument("--paper-id", required=True)
    p_rec.add_argument("--status", required=True)
    p_rec.add_argument("--verdict", default="")
    p_rec.add_argument("--error-sig", default="")
    p_rec.add_argument("--notes", default="")
    p_rec.add_argument("--queue-dir", type=Path, default=DEFAULT_QUEUE_DIR)

    args = ap.parse_args(argv)
    if args.cmd == "index":
        res = build_index(args.papers_root, args.queue_dir, max_archive_entries=args.max_archive)
        print(json.dumps({"counts": res["counts"], "top5_pending": [c for c in res["candidates"] if c["status"] == "pending"][:5]}, indent=2, ensure_ascii=False))
    elif args.cmd == "next":
        try:
            cands = next_candidates(args.queue_dir, tier=args.tier, limit=args.limit)
        except RuntimeError as exc:
            print(json.dumps({"halted": True, "error": str(exc)}, ensure_ascii=False))
            return 2
        print(json.dumps(cands, indent=2, ensure_ascii=False))
    elif args.cmd == "extract":
        print(json.dumps(extract_pdf_text(args.paper_id, args.queue_dir), indent=2, ensure_ascii=False))
    elif args.cmd == "record":
        row = record_status(args.paper_id, args.status, notes=args.notes, error_sig=args.error_sig, verdict=args.verdict, queue_dir=args.queue_dir)
        print(json.dumps(row, indent=2, ensure_ascii=False))
        if row.get("breaker_tripped"):
            return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
