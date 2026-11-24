import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { getSetupStatus } from "./identity/api";
import { Dashboard } from "./identity/Dashboard";
import { FirstAdminWizard } from "./identity/FirstAdminWizard";
import { LoginForm } from "./identity/LoginForm";
import { getSession, type Session } from "./identity/session";

type Gate = "loading" | "error" | "setup" | "login" | "signed-in";

function App() {
  const [gate, setGate] = useState<Gate>("loading");
  const [session, setSession] = useState<Session | null>(null);

  useEffect(() => {
    let active = true;
    async function load() {
      try {
        const status = await getSetupStatus();
        if (!active) return;
        if (status.setup_required) {
          setGate("setup");
          return;
        }
        const current = await getSession();
        if (!active) return;
        if (current) {
          setSession(current);
          setGate("signed-in");
        } else {
          setGate("login");
        }
      } catch {
        if (active) setGate("error");
      }
    }
    void load();
    return () => {
      active = false;
    };
  }, []);

  if (gate === "setup") {
    return (
      <main>
        <FirstAdminWizard onCreated={() => setGate("login")} />
      </main>
    );
  }
  if (gate === "login") {
    return (
      <main>
        <LoginForm
          onSignedIn={(next) => {
            setSession(next);
            setGate("signed-in");
          }}
        />
      </main>
    );
  }
  if (gate === "signed-in" && session) {
    return (
      <main>
        <Dashboard
          session={session}
          onSignedOut={() => {
            setSession(null);
            setGate("login");
          }}
        />
      </main>
    );
  }
  if (gate === "error") {
    return (
      <main>
        <p role="alert">CallHarbor could not be reached. Reload the page to try again.</p>
      </main>
    );
  }
  return <main aria-busy="true" />;
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
