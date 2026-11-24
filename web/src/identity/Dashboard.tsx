import { useState } from "react";
import { AppearanceSettings } from "../design/AppearanceSettings";
import { UserAdmin } from "./UserAdmin";
import { logout, type Session } from "./session";

export interface DashboardProps {
  readonly session: Session;
  readonly onSignedOut: () => void;
}

type View = "users" | "settings";

export function Dashboard({ session, onSignedOut }: DashboardProps) {
  const isAdmin = session.user.role === "admin";
  const [view, setView] = useState<View>(isAdmin ? "users" : "settings");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function handleLogout() {
    setBusy(true);
    setError(null);
    try {
      await logout(session.csrf_token);
      onSignedOut();
    } catch {
      setError("Sign-out failed. Try again.");
      setBusy(false);
    }
  }

  const items: { id: View; label: string }[] = [
    ...(isAdmin ? [{ id: "users" as const, label: "Users" }] : []),
    { id: "settings", label: "Settings" },
  ];

  return (
    <div className="ch-shell">
      <aside className="ch-sidebar" aria-label="Primary">
        <div className="ch-brand">CallHarbor</div>
        <nav className="ch-nav" aria-label="Sections">
          <ul>
            {items.map((item) => (
              <li key={item.id}>
                <button
                  type="button"
                  aria-current={view === item.id ? "page" : undefined}
                  onClick={() => setView(item.id)}
                >
                  {item.label}
                </button>
              </li>
            ))}
          </ul>
        </nav>
      </aside>

      <main className="ch-main">
        <header className="ch-header">
          <div>
            <h1 id="dashboard-heading">CallHarbor</h1>
            <p className="ch-muted">
              Signed in as {session.user.username} ({isAdmin ? "administrator" : "user"})
            </p>
          </div>
          <button type="button" onClick={handleLogout} disabled={busy}>
            {busy ? "Signing out..." : "Sign out"}
          </button>
          {error ? <p role="alert" className="ch-alert">{error}</p> : null}
        </header>

        {view === "users" && isAdmin ? (
          <UserAdmin csrfToken={session.csrf_token} currentUserId={session.user.id} />
        ) : null}
        {view === "settings" ? <AppearanceSettings /> : null}
      </main>
    </div>
  );
}
