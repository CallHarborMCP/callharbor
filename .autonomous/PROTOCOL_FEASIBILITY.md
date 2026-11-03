# CallHarbor v1.0.0 — mandatory early protocol feasibility contract

status=locked_human_authorized
purpose=prove_real_external_compatibility_without_reopening_locked_architecture
owner_issue=CH-001

The product architecture, chosen providers and single-container runtime are ALREADY DECIDED.
CH-001 may not choose a new stack or silently shrink the mandatory V1 product.
Before merging ANY Telnyx/Realtime voice engine feature, CH-001 must deliver
pinned, licensed source citations and executable offline contract tests, and
prove both adapters agree on an explicit, documented audio/message interface.

## Required concrete proof, not another planning document
1. Telnyx official Call Control REST requests, authenticated webhooks with
   raw-body Ed25519 verification and timestamp/replay protection, outbound
   lifecycle, idempotency and expected event-order permutations.
2. Telnyx bidirectional WebSocket handshake and media messages for the
   explicitly selected RTP track and codec. Distinguish the Telnyx inbound,
   outbound and bidirectional notions of track. Prove media clear/mark
   semantics, DTMF, playback delay and timing; record known limits and tests.
3. Realtime provider profile capability matrix: handshake, model/voice,
   input/output codecs and sample rates, session metadata, turn detection,
   input transcription, output audio transcript, cancellation/truncation,
   correlation and error events. Test a supported profile and fail closed
   for unavailable capabilities; 'OpenAI-compatible' does NOT imply audio.
4. Two-party recording source-of-truth: what inbound audio actually arrives
   and what outgoing audio has actually been PLAYED, not merely synthesized.
   Stereo channel alignment, marks, clearing, interruption, partial recording,
   resampling and clock drift must have executable frame-level test fixtures.
5. Transcript fidelity and outcome: model response text may include words
   never played after a barge-in; reconcile audible output with truncation.
   Inbound Realtime ASR is asynchronous/approximate and not word-aligned.
   Retain actual source/event IDs, precise confidence/availability flags,
   and honest approximate timestamps; never synthesize missing speech text.
   If complete two-party transcription cannot be obtained through the selected
   realtime provider without another paid API, report a **V1 blocker** with
   evidence and investigate standards-compliant no-extra-service options.
   Do not silently enable billable STT or mislabel an estimate as verbatim.
6. Security and network: callback HTTPS/public reachability, media WSS,
   scoped WebSocket session authentication, proxy/reconnect, IP/SSRF policy,
   tenant/caller authorization and realistic deployment example.

## Acceptance evidence
- Version-pinned upstream API/schema/event references and attribution.
- Executable independent fake-provider contract fixtures + NEGATIVE tests.
- Measured sample formats, timing and coverage matrix against V1 requirements.
- Reproducible commands and risk/unknown list in a reviewed ADR or fixture doc.
- Green required CI evidence proving the above; mocks must not implement
  the exact service logic under test. No paid calls or hosted AI tests needed.
- CH-009/010/011/012/016/017 and full E2E MUST use this verified contract.
If a required capability is unproven or impossible, block the dependent
work/release explicitly; NEVER declare compatibility by assumption.
