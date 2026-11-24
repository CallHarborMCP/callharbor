import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { getSetupStatus } from "./identity/api";
import { FirstAdminWizard } from "./identity/FirstAdminWizard";
import { LoginForm } from "./identity/LoginForm";

type Gate = "loading" | "error" | "setup" | "login";

function App() {
  const [gate, setGate] = useState<Gate>("loading");

  useEffect(() => {
    let active = true;
    getSetupStatus()
      .then((status) => {
        if (active) setGate(status.setup_required ? "setup" : "login");
      })
      .catch(() => {
        if (active) setGate("error");
      });
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
        <LoginForm onSignedIn={() => window.location.reload()} />
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
