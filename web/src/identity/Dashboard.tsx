import { useState } from "react";
import { UserAdmin } from "./UserAdmin";
import { logout, type Session } from "./session";

export interface DashboardProps {
  readonly session: Session;
  readonly onSignedOut: () => void;
}

export function Dashboard({ session, onSignedOut }: DashboardProps) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const isAdmin = session.user.role === "admin";

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

  return (
    <section aria-labelledby="dashboard-heading">
      <header>
        <h1 id="dashboard-heading">CallHarbor</h1>
        <p>
          Signed in as {session.user.username} ({session.user.role === "admin" ? "administrator" : "user"})
        </p>
        <button type="button" onClick={handleLogout} disabled={busy}>
          {busy ? "Signing out..." : "Sign out"}
        </button>
        {error ? <p role="alert">{error}</p> : null}
      </header>
      {isAdmin ? <UserAdmin csrfToken={session.csrf_token} currentUserId={session.user.id} /> : null}
    </section>
  );
}
