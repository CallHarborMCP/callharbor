import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { calls, type CallRecord } from "../../src/fixtures/calls";
import "./prototype-b.css";

type Tab = "home" | "calls" | "settings" | "admin";

const tabs: { id: Tab; label: string }[] = [
  { id: "home", label: "Home" },
  { id: "calls", label: "Calls" },
  { id: "settings", label: "Settings" },
  { id: "admin", label: "Admin" },
];

function App() {
  const [tab, setTab] = useState<Tab>("home");
  const [open, setOpen] = useState<CallRecord | null>(null);
  const [query, setQuery] = useState("");
  const [recording, setRecording] = useState(false);
  const [density, setDensity] = useState<"comfortable" | "compact">("comfortable");
  const [denied, setDenied] = useState(false);

  const matches = calls.filter((c) =>
    `${c.recipient} ${c.objective}`.toLowerCase().includes(query.trim().toLowerCase()),
  );

  return (
    <div className={`b-app b-${density}`}>
      <header className="b-top">
        <span className="b-logo" aria-hidden="true" />
        <span className="b-title">CallHarbor</span>
        <nav aria-label="Primary" className="b-topnav">
          {tabs.map((t) => (
            <button
              key={t.id}
              type="button"
              aria-current={tab === t.id && !open ? "page" : undefined}
              onClick={() => {
                setOpen(null);
                setTab(t.id);
              }}
            >
              {t.label}
            </button>
          ))}
        </nav>
      </header>

      <main className="b-main">
        {open && (
          <article aria-labelledby="b-detail-title" className="b-card b-detail">
            <button type="button" className="b-link" onClick={() => setOpen(null)}>
              ← All calls
            </button>
            <h1 id="b-detail-title">{open.recipient}</h1>
            <p className="b-muted">{open.objective}</p>
            <dl className="b-facts">
              <div><dt>Started</dt><dd>{open.startedAt}</dd></div>
              <div><dt>Length</dt><dd>{open.duration}</dd></div>
              <div><dt>Outcome</dt><dd>{open.outcome}</dd></div>
              <div><dt>Voice</dt><dd>{open.protocol}</dd></div>
            </dl>
            <h2>Transcript</h2>
            <ol className="b-transcript">
              {open.transcript.map((l) => (
                <li key={l.at + l.text}>
                  <time>{l.at}</time>
                  <div>
                    <span className="b-speaker">{l.speaker}</span>
                    <p>{l.text}</p>
                  </div>
                </li>
              ))}
            </ol>
            {open.recorded ? (
              <audio controls preload="none" aria-label="Call recording" />
            ) : (
              <p className="b-muted">No recording was saved for this call.</p>
            )}
          </article>
        )}

        {!open && tab === "home" && (
          <section aria-labelledby="b-home" className="b-stack">
            <h1 id="b-home">Good afternoon</h1>
            <div className="b-hero-card">
              <p className="b-muted">This week</p>
              <p className="b-big">9 of 14 calls succeeded</p>
              <p className="b-muted">2 calls need your review.</p>
            </div>
            <h2>Latest</h2>
            <ul className="b-list">
              {calls.map((c) => (
                <li key={c.id}>
                  <button type="button" className="b-card b-row" onClick={() => setOpen(c)}>
                    <span>
                      <span className="b-strong">{c.recipient}</span>
                      <span className="b-muted b-block">{c.objective}</span>
                    </span>
                    <span className={`b-pill b-${c.outcome}`}>{c.outcome}</span>
                  </button>
                </li>
              ))}
            </ul>
          </section>
        )}

        {!open && tab === "calls" && (
          <section aria-labelledby="b-calls" className="b-stack">
            <h1 id="b-calls">Calls</h1>
            <label className="b-search">
              <span className="sr-only">Search calls</span>
              <input
                type="search"
                placeholder="Search recipient or objective"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </label>
            {matches.length === 0 ? (
              <div className="b-card" role="status">
                <p className="b-strong">No calls found</p>
                <p className="b-muted">Try a different search.</p>
              </div>
            ) : (
              <ul className="b-list">
                {matches.map((c) => (
                  <li key={c.id}>
                    <button type="button" className="b-card b-row" onClick={() => setOpen(c)}>
                      <span>
                        <span className="b-strong">{c.recipient}</span>
                        <span className="b-muted b-block">{c.startedAt}</span>
                      </span>
                      <span className={`b-pill b-${c.outcome}`}>{c.outcome}</span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </section>
        )}

        {!open && tab === "settings" && (
          <section aria-labelledby="b-settings" className="b-stack">
            <h1 id="b-settings">Appearance</h1>
            <fieldset className="b-card">
              <legend className="b-strong">Density</legend>
              <div className="b-segmented" role="radiogroup" aria-label="Density">
                {(["comfortable", "compact"] as const).map((d) => (
                  <label key={d}>
                    <input
                      type="radio"
                      name="density"
                      value={d}
                      checked={density === d}
                      onChange={() => setDensity(d)}
                    />
                    <span>{d === "comfortable" ? "Comfortable" : "Compact"}</span>
                  </label>
                ))}
              </div>
            </fieldset>
            <div className="b-card b-row">
              <span>
                <span className="b-strong">Record calls locally</span>
                <span className="b-muted b-block">Off by default. You are responsible for any consent notice in your call instructions.</span>
              </span>
              <input
                type="checkbox"
                role="switch"
                aria-label="Record calls locally"
                checked={recording}
                onChange={(e) => setRecording(e.target.checked)}
              />
            </div>
          </section>
        )}

        {!open && tab === "admin" && (
          <section aria-labelledby="b-admin" className="b-stack">
            <h1 id="b-admin">Users</h1>
            {denied ? (
              <div className="b-card" role="alert">
                <p className="b-strong">Administrator access required</p>
                <button type="button" onClick={() => setDenied(false)}>Back</button>
              </div>
            ) : (
              <>
                <p className="b-muted">Two administrators · three users</p>
                <button type="button" onClick={() => setDenied(true)}>Simulate non-admin</button>
              </>
            )}
          </section>
        )}
      </main>
    </div>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
