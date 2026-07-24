# Latency Model (v2 — Parallel Warm with Mid-Flight Interception)

How this system reasons about latency. **This is the v2 model** — it deliberately
*embraces* speculative LLM warming and halts it mid-utterance on a
high-confidence cache match. That is the opposite choice from a sequential /
no-warm design (which serves `$0` LLM on hits and masks miss-latency with a filler
clip). Read this alongside plan §3–§4 (the state machine) and §7–§8 (cost table +
load-shedding).

---

## 1. What latency the customer actually feels

**Perceived latency = end-of-customer-speech → first audible agent response.**

The customer's mental timer starts when they stop talking. Anything done *while
they are still speaking* is free — hidden under their own speech. So the only
latency worth optimizing hard is the **post-end-of-utterance** gap, and the prize
is to arrive at end-of-utterance with as much work already done as possible.

This is the entire justification for v2: warm the LLM *during* speech so a cache
**miss** can decode immediately instead of cold-prefilling after the customer
stops.

## 2. Dispatch decides on the FINAL transcript; warming does not wait

Two things run in parallel from the first **stable** STT token (plan §4):

- **Warming (Branch A):** speculative prefill of query tokens into the KV cache.
  Runs *during* speech.
- **Dispatch decision (Branch B):** the authoritative intent/cache match — made on
  the **final** transcript, never on a partial (a partial is an incomplete intent;
  acting on it is unsafe, especially for money-movement).

The split is the key idea: **the decision waits for the final; the warm-up does
not.** That's what reclaims the speech-time overlap without acting prematurely.

## 3. The mid-flight halt (and why it needs an *extreme* threshold)

When Branch B produces an **extreme-confidence** action match mid-utterance, the
supervisor **halts Branch A** (`context.CancelFunc` → abort prefill, reclaim GPU)
and commits to the cache path — but still executes the bank action only after the
final transcript confirms (writes require explicit confirmation).

The halt bar is deliberately high (`EXTREME`, e.g. 0.97, above the `NORMAL` 0.94
dispatch threshold) because **un-halting is expensive**: if you halt on a merely
moderate early match and the completed utterance diverges, you've discarded the
warm KV cache and must **cold-prefill at end-of-utterance** — the worst case.
Moderate matches keep *both* branches alive and defer to the final decision.

## 4. Static-prefix prewarming (free, do it regardless)

The expensive prefill is the **system prompt**, identical for every turn/user.
Prefix-cache its KV **once** and reuse it across all turns (llama.cpp/Ollama
prompt caching). This is a one-time cost, not per-turn waste, and it shrinks the
prefill on every turn regardless of hit/miss. Only per-session history + the short
query remain as per-turn prefill.

## 5. The honest cost — warming spends GPU on hits

This is the tension v2 accepts. Parallel warming prefills the LLM on turns that
turn out to be cache **hits** — the very turns the cache exists to keep off the
LLM. The mid-flight halt *reduces* this waste (it stops as soon as it's confident)
in proportion to how *early* the extreme match fires, but does **not** eliminate
it.

| Turn outcome | Sequential / no-warm baseline | v2 (warm + mid-flight halt) |
|---|---|---|
| Hit, caught early | `$0` LLM | prefill up to halt point only, then reclaimed |
| Hit, caught only at final | `$0` LLM | *full* query prefill wasted |
| Miss | cold prefill after VAD, **masked by filler** | warm → decode immediately, **no filler** |
| False early halt | n/a | worst case: cold re-prefill at final |

**Why no reliable shortcut exists:** you can't cheaply predict hit-vs-miss before
the query is essentially complete. An *early strong match* reliably says "hit →
halt"; an *early non-match* says almost nothing (it's the default state of an
incomplete query — nearly every eventual hit also fails to match until the last
word). So there is no reliable early "this will miss, warm it" signal beyond
"we're warming everything unless a hit halts us." v2 chooses to warm-by-default
and halt on confidence; that is the cost it pays for miss-latency.

## 6. Load-shedding: warming is a knob (plan §8)

Because warming costs GPU on hits, it must be turn-off-able under pressure and
degrade to the sequential/no-warm baseline (decide-on-final + filler):

- **GPU-pressure backpressure:** above a utilization watermark, disable Branch A.
  No correctness change — only the latency profile degrades.
- **Per-tenant / per-route policy:** warm only where miss-latency matters.
- **Keyword pre-gate (optional):** a cheap deterministic keyword spotter on the
  first 1–2 words may suppress warming when a strong transactional keyword appears
  (likely hit). Use **only** as a warm/don't-warm hint — never as the dispatch
  mechanism (dispatch stays semantic on the final transcript, so paraphrases
  without keywords still hit).

## 7. The load-bearing assumption

The GPU reclaim depends on **Ollama cancelling an in-flight prefill cleanly**. If
it can't, the halt degrades to "let it finish, discard the result" — correct, but
the reclaim is lost and the wasted-prefill cost rises. **Validate cancellation
before building the state machine.**

## 8. Instrument (decide thresholds from data)

- **Perceived latency** end-of-speech → first TTS audio, bucketed by
  {hit-early, hit-final, miss, false-halt}.
- **Wasted-prefill ratio** — prefill tokens spent on turns that ended as cache
  hits ÷ total prefill. *The number that justifies or kills v2.*
- **Early-halt precision** — of turns halted on `EXTREME`, how many the final
  confirmed (low → `EXTREME` too loose; false-halt cold-restarts hurt).
- **Cache hit-rate**, **intent-type distribution**, **GPU utilization** vs. the
  load-shed watermark.

Tune `EXTREME` / `NORMAL` and the watermark from these. **If the wasted-prefill
ratio is high and miss-latency isn't actually hurting UX, the sequential/no-warm
baseline is the correct architecture and v2 should be shelved.**

---

## Summary

- Optimize only the post-end-of-utterance gap; overlap the rest under speech.
- **Warm during speech, decide on the final transcript** — the decision waits, the
  warm-up doesn't.
- Halt the warm branch only on an **extreme**-confidence match (un-halting is
  expensive); execute bank actions only after the final confirms.
- Prewarm the shared static prefix (free); accept that per-turn query warming
  **spends GPU on hits** — that is v2's deliberate trade for fast misses.
- Keep warming **load-sheddable** back to the no-warm baseline, and let the
  **wasted-prefill ratio** decide whether v2 earns its keep.
