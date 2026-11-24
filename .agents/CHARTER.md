# CallHarbor project charter

CallHarbor is a production-grade, lightweight, self-hosted, multi-user platform for autonomous outbound AI telephone calls, with a complete WebGUI and remote MCP integration.

The product and engineering contracts define its capabilities and invariants. GitHub issues define authorized changes and release scope; agents choose and verify the implementation within those boundaries. Do not turn a feature into a prototype, silently defer acceptance criteria, weaken product requirements, or claim success without evidence.

Keep a Go modular monolith with embedded React, SQLite and a single production container. Maintain independent telephony and voice protocol interfaces; do not require an additional paid planning model or an external runtime. See PRODUCT_SCOPE.md and ARCHITECTURE.md for the current product design.

Owner-authorized issues can explicitly evolve product contracts. Keep the affected contracts and lock manifest consistent with the approved change in the same reviewed PR.
