# Documentation and product evidence policy — immutable

status=locked_human_authorized
agent_must_not_edit=this_file

Public user/admin/operator/developer docs live in `docs/`; the locked governance, product contract, design constraints and agent workflow live in `.autonomous/`. `AGENTS.md` is only the entry point. Project internals do not belong in public onboarding docs, especially user-specific agent prompts.
- Include first admin wizard, standalone accounts/OIDC, MCP/ChatGPT authorization, per-user Telnyx and Realtime provider configuration, reverse proxy/WebSocket ingress, call/recording workflows, safe retention and warning, backups/restore/upgrade/rollback, diagnostics, threat model and development/test instructions.
- README is product-quality: features, honest capabilities and limits, Docker Compose quickstart, tested prerequisites, exact commands, auth flow, image tags, docs links and actual screenshots.
- Screenshots must be captured through Playwright from the genuine production-built running application with synthetic test fixtures. Save to `docs/assets/screenshots/`, commit images, embed in README, and verify mobile/desktop and light/dark screenshots against live pages. No illustrations/mockups posed as screenshots.
- At release include professional `docs/releases/v1.0.0.md` with real features, known limitations, upgrade path, checksums, registry artifacts and clear simulated-versus-live Telnyx verification disclosure.
- No fabricated human testing, customer testimonials, prices, benchmark measurements or claims of real-world Telnyx compatibility without a real call. Documentation commands themselves must be tested in a clean environment.
- Keep public product docs in English (same as WebGUI), while call speech language remains configurable.
