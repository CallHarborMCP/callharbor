import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { calls, type CallRecord } from "../../src/fixtures/calls";
import "./prototype-a.css";

type View = "dashboard" | "calls" | "detail" | "settings" | "admin" | "forbidden";

const nav: { id: View; label: string }[] = [
  { id: "dashboard", label: "Dashboard" },
  { id: "calls", label: "Calls" },
  { id: "settings", label: "Settings" },
  { id: "admin", label: "Admin" },
];

function App() {
  const [view, setView] = useState<View>("dashboard");
  const [selected, setSelected] = useState<CallRecord>(calls[0]);
  const [filter, setFilter] = useState<"all" | "ended" | "failed">("all");
  const [recording, setRecording] = useState(false);

  const open = (call: CallRecord) => {
    setSelected(call);
    setView("detail");
  };
  const visible = calls.filter((c) => filter === "all" || c.status === filter);
  const activeNav = view === "detail" ? "calls" : view;

  return (
    <div className="a-shell">
      <aside className="a-sidebar" aria-label="Primary">
        <div className="a-brand">CallHarbor</div>
        <nav>
          <ul>
            {nav.map((n) => (
              <li key={n.id}>
                <button
                  type="button"
                  aria-current={activeNav === n.id ? "page" : undefined}
                  onClick={() => setView(n.id)}
                >
                  {n.label}
                </button>
              </li>
            ))}
          </ul>
        </nav>
      </aside>

      <main className="a-main">
        {view === "dashboard" && (
          <section aria-labelledby="h-dash">
            <h1 id="h-dash">Overview</h1>
            <div className="a-stats">
              <Stat label="Calls this week" value="14" />
              <Stat label="Successful" value="9" />
              <Stat label="Needs review" value="2" />
            </div>
            <CallTable rows={calls} onOpen={open} caption="Recent calls" />
          </section>
        )}

        {view === "calls" && (
          <section aria-labelledby="h-calls">
            <h1 id="h-calls">Calls</h1>
            <div className="a-filters" role="group" aria-label="Filter by status">
              {(["all", "ended", "failed"] as const).map((f) => (
                <button
                  key={f}
                  type="button"
                  aria-pressed={filter === f}
                  onClick={() => setFilter(f)}
                >
                  {f === "all" ? "All" : f === "ended" ? "Ended" : "Failed"}
                </button>
              ))}
            </div>
            {visible.length === 0 ? (
              <p className="a-empty" role="status">No calls match this filter.</p>
            ) : (
              <CallTable rows={visible} onOpen={open} caption="Calls" />
            )}
          </section>
        )}

        {view === "detail" && (
          <section aria-labelledby="h-detail">
            <button type="button" className="a-back" onClick={() => setView("calls")}>
              Back to calls
            </button>
            <h1 id="h-detail">{selected.recipient}</h1>
            <p className="a-muted">
              {selected.to} · {selected.startedAt} · {selected.duration}
            </p>
            <div className="a-grid">
              <div className="a-panel">
                <h2>Transcript</h2>
                <ol className="a-transcript">
                  {selected.transcript.map((line) => (
                    <li key={line.at + line.text}>
                      <span className="a-time">{line.at}</span>
                      <strong>{line.speaker}:</strong> {line.text}
                    </li>
                  ))}
                </ol>
              </div>
              <div className="a-panel">
                <h2>Timeline</h2>
                <ul>
                  {selected.timeline.map((e) => (
                    <li key={e.at}>
                      <span className="a-time">{e.at}</span> {e.label}
                    </li>
                  ))}
                </ul>
                <p>Outcome: <strong>{selected.outcome}</strong></p>
                {selected.recorded ? (
                  <audio controls preload="none" aria-label="Call recording" />
                ) : (
                  <p className="a-muted">No recording was saved for this call.</p>
                )}
              </div>
            </div>
          </section>
        )}

        {view === "settings" && (
          <section aria-labelledby="h-settings">
            <h1 id="h-settings">Appearance and recording</h1>
            <div className="a-panel">
              <label className="a-row">
                <span>Record calls locally</span>
                <input
                  type="checkbox"
                  role="switch"
                  checked={recording}
                  onChange={(e) => setRecording(e.target.checked)}
                  aria-describedby="rec-help"
                />
              </label>
              <p id="rec-help" className="a-muted">
                Recording is off by default. Recording calls may require notifying or obtaining consent from
                the other party. You are responsible for compliance.
              </p>
            </div>
          </section>
        )}

        {view === "admin" && (
          <section aria-labelledby="h-admin">
            <h1 id="h-admin">Users</h1>
            <p className="a-muted">Admin view. Access is enforced server-side for this role.</p>
            <button type="button" onClick={() => setView("forbidden")}>Simulate non-admin</button>
          </section>
        )}

        {view === "forbidden" && (
          <section aria-labelledby="h-forbidden" role="alert">
            <h1 id="h-forbidden">You do not have access</h1>
            <p className="a-muted">Ask an administrator to change your role.</p>
            <button type="button" onClick={() => setView("dashboard")}>Return to overview</button>
          </section>
        )}
      </main>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="a-stat">
      <div className="a-stat-value">{value}</div>
      <div className="a-muted">{label}</div>
    </div>
  );
}

function CallTable({
  rows,
  onOpen,
  caption,
}: {
  rows: readonly CallRecord[];
  onOpen: (c: CallRecord) => void;
  caption: string;
}) {
  return (
    <>
      <div className="a-table-wrap">
        <table>
          <caption className="sr-only">{caption}</caption>
          <thead>
            <tr>
              <th scope="col">Recipient</th>
              <th scope="col">Objective</th>
              <th scope="col">Started</th>
              <th scope="col">Outcome</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((c) => (
              <tr key={c.id} onClick={() => onOpen(c)}>
                <td>
                  <button type="button" onClick={() => onOpen(c)}>{c.recipient}</button>
                </td>
                <td>{c.objective}</td>
                <td>{c.startedAt}</td>
                <td>{c.outcome}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ul className="a-cards" aria-label={caption}>
        {rows.map((c) => (
          <li key={c.id}>
            <button type="button" onClick={() => onOpen(c)}>
              <span className="a-card-title">{c.recipient}</span>
              <span className="a-muted">{c.objective}</span>
              <span className="a-muted">{c.startedAt} · {c.outcome}</span>
            </button>
          </li>
        ))}
      </ul>
    </>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
