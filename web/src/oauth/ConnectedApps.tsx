import { useEffect, useId, useRef, useState } from "react";
import { listConnectedApps, OAuthError, revokeConnectedApp, type ConnectedApp } from "./api";
import { describeScope, formatDate, hostOf } from "./format";

export interface ConnectedAppsProps {
  readonly csrfToken: string;
}

const messages: Record<string, string> = {
  not_found: "That app is no longer connected. The list has been refreshed.",
  csrf_invalid: "Your session could not be verified. Reload the page and try again.",
  network: "The server could not be reached. Check your connection and try again.",
  internal: "Something went wrong. Try again.",
};

function describe(error: unknown): string {
  const code = error instanceof OAuthError ? error.code : "internal";
  return messages[code] ?? messages.internal;
}

export function ConnectedApps({ csrfToken }: ConnectedAppsProps) {
  const [apps, setApps] = useState<ConnectedApp[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const [confirmingId, setConfirmingId] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState("");
  const headingRef = useRef<HTMLHeadingElement>(null);
  const headingId = useId();

  useEffect(() => {
    let active = true;
    listConnectedApps()
      .then((loaded) => {
        if (!active) return;
        setApps(loaded);
        setLoadError(null);
      })
      .catch((caught: unknown) => {
        if (active) setLoadError(describe(caught));
      });
    return () => {
      active = false;
    };
  }, [reloadKey]);

  async function revoke(app: ConnectedApp) {
    setRevokingId(app.id);
    setActionError(null);
    setNotice("");
    try {
      await revokeConnectedApp(csrfToken, app.id);
      setConfirmingId(null);
      setApps((current) => current?.filter((item) => item.id !== app.id) ?? null);
      setNotice(`Access for ${app.client_name || "the app"} was revoked. It can no longer call CallHarbor.`);
      headingRef.current?.focus();
    } catch (caught) {
      setActionError(describe(caught));
      if (caught instanceof OAuthError && caught.code === "not_found") {
        setConfirmingId(null);
        setReloadKey((key) => key + 1);
      }
    } finally {
      setRevokingId(null);
    }
  }

  return (
    <section aria-labelledby={headingId} className="ch-panel">
      <h2 id={headingId} ref={headingRef} tabIndex={-1}>
        Connected apps
      </h2>
      <p className="ch-muted">
        Apps you have allowed to use CallHarbor on your behalf. Revoking access stops the app straight away.
      </p>

      <p role="status" className="ch-status">{notice}</p>
      {actionError ? <p role="alert" className="ch-alert">{actionError}</p> : null}

      {loadError ? (
        <>
          <p role="alert" className="ch-alert">{loadError}</p>
          <button
            type="button"
            onClick={() => {
              setLoadError(null);
              setApps(null);
              setReloadKey((key) => key + 1);
            }}
          >
            Try again
          </button>
        </>
      ) : apps === null ? (
        <p aria-busy="true">Loading connected apps...</p>
      ) : apps.length === 0 ? (
        <ConnectEmptyState />
      ) : (
        <ul className="ch-grant-list">
          {apps.map((app) => (
            <GrantItem
              key={app.id}
              app={app}
              confirming={confirmingId === app.id}
              revoking={revokingId === app.id}
              anyRevoking={revokingId !== null}
              onStart={() => {
                setActionError(null);
                setNotice("");
                setConfirmingId(app.id);
              }}
              onCancel={() => setConfirmingId(null)}
              onConfirm={() => void revoke(app)}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

interface GrantItemProps {
  readonly app: ConnectedApp;
  readonly confirming: boolean;
  readonly revoking: boolean;
  readonly anyRevoking: boolean;
  readonly onStart: () => void;
  readonly onCancel: () => void;
  readonly onConfirm: () => void;
}

function GrantItem({ app, confirming, revoking, anyRevoking, onStart, onCancel, onConfirm }: GrantItemProps) {
  const name = app.client_name || "Unnamed app";
  const identity = hostOf(app.client_id) ?? app.client_id;
  const confirmId = useId();
  const confirmRef = useRef<HTMLDivElement>(null);
  const startRef = useRef<HTMLButtonElement>(null);

  // Focus follows the interaction: into the confirmation when it opens, back
  // to the trigger when the user keeps access.
  const wasConfirming = useRef(false);
  useEffect(() => {
    if (confirming) {
      confirmRef.current?.focus();
    } else if (wasConfirming.current) {
      startRef.current?.focus();
    }
    wasConfirming.current = confirming;
  }, [confirming]);

  return (
    <li className="ch-grant">
      <div className="ch-grant-main">
        <p className="ch-grant-name">{name}</p>
        <p className="ch-muted ch-break">
          <code>{identity}</code>
        </p>
        <ul className="ch-scope-list ch-scope-list--compact">
          {app.scopes.map((scope) => (
            <li key={scope}>
              <span>{describeScope(scope)}</span>
              <code className="ch-muted">{scope}</code>
            </li>
          ))}
        </ul>
        <p className="ch-muted ch-grant-dates">
          Connected {formatDate(app.created_at)}
          <br />
          {app.last_used_at ? `Last used ${formatDate(app.last_used_at)}` : "Not used yet"}
        </p>
      </div>

      {confirming ? (
        <div
          ref={confirmRef}
          tabIndex={-1}
          role="group"
          aria-labelledby={confirmId}
          className="ch-grant-confirm"
        >
          <p id={confirmId}>
            Revoke access for <strong>{name}</strong>? It stops working immediately and needs your approval again to
            reconnect.
          </p>
          <div className="ch-consent-actions">
            <button type="button" onClick={onCancel} disabled={revoking}>
              Keep access
            </button>
            <button type="button" className="ch-button-danger" onClick={onConfirm} disabled={revoking}>
              {revoking ? "Revoking..." : "Revoke access"}
            </button>
          </div>
        </div>
      ) : (
        <button
          ref={startRef}
          type="button"
          className="ch-grant-action"
          aria-label={`Revoke access for ${name}`}
          onClick={onStart}
          disabled={anyRevoking}
        >
          Revoke access
        </button>
      )}
    </li>
  );
}

function ConnectEmptyState() {
  const serverURL = `${window.location.origin}/mcp`;
  return (
    <div className="ch-empty">
      <p>No apps are connected yet.</p>
      <p className="ch-muted">
        To connect an MCP client such as ChatGPT or Claude, add this server address in the client's connector or MCP
        settings. Approve the request here when it appears.
      </p>
      <p className="ch-break">
        <code>{serverURL}</code>
      </p>
    </div>
  );
}
