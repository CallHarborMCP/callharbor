import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { AppearanceProvider } from "./design/AppearanceProvider";
import "./design/design-system.css";
import { getSetupStatus } from "./identity/api";
import { Dashboard } from "./identity/Dashboard";
import { FirstAdminWizard } from "./identity/FirstAdminWizard";
import { LoginForm } from "./identity/LoginForm";
import { getSession, type Session } from "./identity/session";
import { ConsentScreen } from "./oauth/ConsentScreen";
import { isConsentPath, requestIdFromSearch } from "./oauth/route";

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

  return (
    <AppearanceProvider
      userId={gate === "signed-in" && session ? session.user.id : null}
      csrfToken={gate === "signed-in" && session ? session.csrf_token : null}
    >
      <Screen
        gate={gate}
        session={session}
        onCreated={() => setGate("login")}
        onSignedIn={(next) => {
          setSession(next);
          setGate("signed-in");
        }}
        onSignedOut={() => {
          setSession(null);
          setGate("login");
        }}
      />
    </AppearanceProvider>
  );
}

interface ScreenProps {
  readonly gate: Gate;
  readonly session: Session | null;
  readonly onCreated: () => void;
  readonly onSignedIn: (session: Session) => void;
  readonly onSignedOut: () => void;
}

function Screen({ gate, session, onCreated, onSignedIn, onSignedOut }: ScreenProps) {
  if (gate === "setup") {
    return (
      <main>
        <FirstAdminWizard onCreated={onCreated} />
      </main>
    );
  }
  if (gate === "login") {
    return (
      <main>
        <LoginForm onSignedIn={onSignedIn} />
      </main>
    );
  }
  if (gate === "signed-in" && session) {
    // The consent route keeps its query string through setup and sign-in, so
    // the same request is shown once the user is signed in.
    if (isConsentPath(window.location.pathname)) {
      return (
        <ConsentScreen
          session={session}
          requestId={requestIdFromSearch(window.location.search)}
          onSignedOut={onSignedOut}
        />
      );
    }
    return <Dashboard session={session} onSignedOut={onSignedOut} />;
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
