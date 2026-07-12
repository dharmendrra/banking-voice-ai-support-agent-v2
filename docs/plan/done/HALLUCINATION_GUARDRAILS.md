# Hallucination Guardrails (No-RAG Design)

**Decision (2026-07-10): no RAG.** The system does not ground the LLM in bank
documents. Correct facts come only from deterministic sources — the **action/API
path** (account data) and the **curated FAQ index** (top informational answers).
The LLM fallthrough is therefore **not a question-answerer**; it is a
**safe deflector**.

This document explains what that means and the guardrails that enforce it.

---

## Where facts come from

| Fact type | Source | Guaranteed correct? |
|-----------|--------|--------------------|
| Account data (balance, transactions, due dates) | Bank API (action path), live per turn | Yes — deterministic |
| Top informational Q&A (hours, how to open an account) | Curated FAQ index (Qdrant) | Yes — pre-authored |
| Long-tail informational / product specifics | **Not answered** — escalate to human | n/a |
| Anything the LLM would assert on its own | **Blocked** | — |

**Consequence to accept:** informational coverage = exactly what is curated in
the FAQ index. The long tail escalates. This is the trade for skipping RAG:
safer and simpler, but more escalations. Revisit (add RAG) only if measured
informational-escalation volume is too high.

## Why the LLM still hallucinates without a job change

If the LLM fallthrough were allowed to answer bank questions freely, with no
grounding it would confidently fabricate:

- **Account facts** — invent a balance or due date it has no access to.
- **Product/policy facts** — invent an APR, fee, limit, or eligibility rule
  (bank-specific and time-varying — not in any model's weights).
- **Procedures** — invent dispute steps, phone numbers, or deadlines.
- **Advice** — give personalized financial advice it is not licensed to give.

A better model lowers the *rate* and refuses more reliably, but cannot *know*
these facts. So the fix is not model quality — it is **containment**.

## The guardrail set (containment)

1. **Trusted-source rule.** The LLM may only relay facts that came from a bank
   API result or a curated FAQ entry. It must never assert a rate / fee /
   balance / procedure on its own.
2. **Output filter (backstop).** Before TTS, block any specific value (balance,
   rate, fee, date, account number) that did not originate from a trusted
   source. An un-sourced number → suppress → escalate.
3. **Public/private separation.** Never speak an account-specific value except
   from the API path. The system prompt forbids first-person account claims.
4. **Scope / advice refusal.** Financial advice or out-of-scope intents → refuse
   and offer a human / qualified advisor.
5. **Escalate-to-human as the default miss behavior** for anything factual or
   uncertain. A refusal + handoff always beats a confident wrong answer.
6. **System-prompt role-limit.** Constrain the LLM to conversational glue —
   greetings, clarifying questions, "let me connect you" — and explicitly forbid
   factual bank claims.

## What the LLM fallthrough IS allowed to do

- Acknowledge and clarify ("Just to confirm, you'd like your current balance?").
- Handle small talk / conversational framing.
- Produce safe deflections and escalation phrasing.
- **Context-resolution (follow-ups):** rewrite a context-dependent utterance
  ("and last month's?") into a **standalone request** ("transactions for last
  month") that **re-enters action/FAQ dispatch**. This is safe because the LLM
  emits a *query to re-dispatch*, **not a stated fact** — the resolved query still
  goes through the action path (which fetches real data) and all the guardrails
  below. The LLM resolves; it never fulfils.
- **Not**: state any bank fact, number, rate, procedure, or advice.

> **The LLM has two roles, both safe:** (1) *deflector* for genuinely unmatched
> queries; (2) *context-resolver* that rewrites follow-ups back into dispatch. In
> neither role does it produce a customer-facing bank fact — role (2)'s output is
> a re-dispatched query, subject to the same action-path + guardrail checks.

## Instrument

- **Informational-escalation rate** — how often the long tail hits the human
  handoff. If high, that is the signal to reconsider adding RAG.
- **Output-filter trips** — how often the LLM tried to emit an un-sourced value
  (indicates prompt/role tuning needed).
- **Advice/scope refusals** — volume and false-positive rate.

## Summary

RAG was declined. Correctness comes from deterministic sources only; the LLM is
contained to a **deflector + context-resolver** role — it rewrites follow-ups
back into dispatch but never states a bank fact itself. The guardrails convert
"confidently wrong" into "refuse and escalate." Coverage of long-tail
informational questions is intentionally traded away for safety and simplicity,
and is measurable — add RAG
later only if the data demands it.
