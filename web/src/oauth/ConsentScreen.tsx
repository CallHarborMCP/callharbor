import { useEffect, useEffectEvent, useRef, useState, type ReactNode } from "react";
import { logout, type Session } from "../identity/session";
import { decideConsent, getConsentRequest, OAuthError, type ConsentRequest } from "./api";
import { describeScope, hostOf, isHttpsURL } from "./format";

export interface ConsentScreenProps {
  readonly session: Session;
  /** The ?request= value, or null when the link has none. */
  readonly requestId: string | null;
  /** Clears the session and returns to the sign-in gate. */
  readonly onSignedOut: () => void;
  /** Sends the browser to the redirect target. Defaults to window.location.assign. */
  readonly navigate?: (url: string) => void;
}

const messages: Record<string, string> = {
  not_found: "This request has expired or was already answered. Return to the app and start again.",
  csrf_invalid: "Your session could not be verified. Reload the page and try again.",
  invalid_input: "The decision was rejected. Reload the page and try again.",
  network: "The server could not be reached. Check your connection and try again.",
  internal: "Something went wrong. Try again.",
};

function describe(error: unknown): string {
  const code = error instanceof OAuthError ? error.code : "internal";
  return messages[code] ?? messages.internal;
}

const assign = (url: string) => window.location.assign(url);

export function ConsentScreen({ requestId, ...rest }: ConsentScreenProps) {
  if (requestId === null) {
    return (
      <ConsentFrame heading="Connection request unavailable" phase="unavailable">
        <p role="alert">This link is incomplete. Return to the app and start again.</p>
      </ConsentFrame>
    );
  }
  return <ConsentRequestView key={requestId} requestId={requestId} {...rest} />;
}

type Load =
  | { readonly status: "loading" }
  | { readonly status: "ready"; readonly request: ConsentRequest }
  | { readonly status: "gone" }
  | { readonly status: "failed"; readonly message: string };

type Decision = "approve" | "deny";

function ConsentRequestView({
  session,
  requestId,
  onSignedOut,
  navigate = assign,
}: Omit<ConsentScreenProps, "requestId"> & { readonly requestId: string }) {
  const [load, setLoad] = useState<Load>({ status: "loading" });
  const [attempt, setAttempt] = useState(0);
  const [deciding, setDeciding] = useState<Decision | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [signingOut, setSigningOut] = useState(false);

  // Session loss is handled by the parent; it is not a reason to refetch.
  const signedOut = useEffectEvent(() => onSignedOut());

  useEffect(() => {
    let active = true;
    getConsentRequest(requestId)
      .then((request) => {
        if (active) setLoad({ status: "ready", request });
      })
      .catch((caught: unknown) => {
        if (!active) return;
        if (caught instanceof OAuthError && caught.code === "unauthenticated") {
          signedOut();
        } else if (caught instanceof OAuthError && caught.code === "not_found") {
          setLoad({ status: "gone" });
        } else {
          setLoad({ status: "failed", message: describe(caught) });
        }
      });
    return () => {
      active = false;
    };
  }, [requestId, attempt]);

  if (load.status === "loading") {
    return (
      <ConsentFrame heading="Allow app access to CallHarbor" phase="loading" busy>
        <p>Loading the request...</p>
      </ConsentFrame>
    );
  }

  if (load.status === "gone") {
    return (
      <ConsentFrame heading="Connection request unavailable" phase="unavailable">
        <p role="alert">{messages.not_found}</p>
      </ConsentFrame>
    );
  }

  if (load.status === "failed") {
    return (
      <ConsentFrame heading="Allow app access to CallHarbor" phase="failed">
        <p role="alert">{load.message}</p>
        <button
          type="button"
          onClick={() => {
            setLoad({ status: "loading" });
            setAttempt((count) => count + 1);
          }}
        >
          Try again
        </button>
      </ConsentFrame>
    );
  }

  const { request } = load;
  return (
    <ConsentDecision
      request={request}
      session={session}
      deciding={deciding}
      actionError={actionError}
      signingOut={signingOut}
      onDecide={async (approve) => {
        setDeciding(approve ? "approve" : "deny");
        setActionError(null);
        try {
          navigate(await decideConsent(session.csrf_token, request.id, approve));
        } catch (caught) {
          setDeciding(null);
          if (caught instanceof OAuthError && caught.code === "unauthenticated") {
            onSignedOut();
          } else if (caught instanceof OAuthError && caught.code === "not_found") {
            setLoad({ status: "gone" });
          } else {
            setActionError(describe(caught));
          }
        }
      }}
      onSwitchAccount={async () => {
        setSigningOut(true);
        setActionError(null);
        try {
          await logout(session.csrf_token);
          onSignedOut();
        } catch {
          setSigningOut(false);
          setActionError("Sign-out failed. Try again.");
        }
      }}
    />
  );
}

interface ConsentDecisionProps {
  readonly request: ConsentRequest;
  readonly session: Session;
  readonly deciding: Decision | null;
  readonly actionError: string | null;
  readonly signingOut: boolean;
  readonly onDecide: (approve: boolean) => void;
  readonly onSwitchAccount: () => void;
}

function ConsentDecision({ request, session, deciding, actionError, signingOut, onDecide, onSwitchAccount }: ConsentDecisionProps) {
  const identity = request.client_host || hostOf(request.client_id) || request.client_id;
  const returnHost = hostOf(request.redirect_uri) ?? "the app";
  const busy = deciding !== null || signingOut;

  return (
    <ConsentFrame heading="Allow app access to CallHarbor" phase="ready" busy={busy}>
      <section aria-labelledby="consent-app-heading" className="ch-consent-block">
        <h2 id="consent-app-heading">App</h2>
        <p className="ch-app-name">{request.client_name || "Unnamed app"}</p>
        <p className="ch-muted">The app supplied this name. CallHarbor has not verified it.</p>

        <p className="ch-identity">
          <span className="ch-muted">{request.metadata_document ? "Domain" : "Registered as"}</span>
          <code className="ch-identity-value">{identity}</code>
        </p>
        {request.metadata_document ? (
          <p>
            This app is identified by the domain shown above. Allow only if you trust that domain.
          </p>
        ) : (
          <p>
            This app registered itself with CallHarbor. Its name and address were not checked, and anyone can
            register under any name. Allow only if you started this connection yourself.
          </p>
        )}
        {request.client_uri && isHttpsURL(request.client_uri) ? (
          <p>
            Website:{" "}
            <a href={request.client_uri} target="_blank" rel="noopener noreferrer">
              {hostOf(request.client_uri)}
              <span className="sr-only"> (opens in a new tab)</span>
            </a>
          </p>
        ) : null}
      </section>

      <section aria-labelledby="consent-scopes-heading" className="ch-consent-block">
        <h2 id="consent-scopes-heading">Permissions requested</h2>
        <ul className="ch-scope-list">
          {request.scopes.map((scope) => (
            <li key={scope}>
              <span>{describeScope(scope)}</span>
              <code className="ch-muted">{scope}</code>
            </li>
          ))}
        </ul>
      </section>

      <section aria-labelledby="consent-return-heading" className="ch-consent-block">
        <h2 id="consent-return-heading">After you decide</h2>
        <p>
          CallHarbor will send you back to <strong>{returnHost}</strong>.
        </p>
      </section>

      {actionError ? <p role="alert" className="ch-alert">{actionError}</p> : null}
      {deciding ? <p role="status">Sending you back to the app...</p> : null}

      <div className="ch-consent-actions">
        <button type="button" className="ch-button-secondary" onClick={() => onDecide(false)} disabled={busy}>
          {deciding === "deny" ? "Denying..." : "Deny"}
        </button>
        <button type="button" className="ch-button-primary" onClick={() => onDecide(true)} disabled={busy}>
          {deciding === "approve" ? "Allowing..." : "Allow"}
        </button>
      </div>

      <p className="ch-consent-footer">
        Signed in as {session.user.username}.{" "}
        <button type="button" className="ch-link-button" onClick={onSwitchAccount} disabled={busy}>
          {signingOut ? "Signing out..." : "Not you? Sign out"}
        </button>
      </p>
      <p className="ch-muted">You can end this access at any time under Connected apps.</p>
    </ConsentFrame>
  );
}

interface ConsentFrameProps {
  readonly heading: string;
  readonly phase: "loading" | "ready" | "failed" | "unavailable";
  readonly busy?: boolean;
  readonly children: ReactNode;
}

// ConsentFrame moves focus to its heading whenever the phase changes, so
// screen-reader and keyboard users land on the new content.
function ConsentFrame({ heading, phase, busy = false, children }: ConsentFrameProps) {
  const headingRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    headingRef.current?.focus();
  }, [phase]);

  return (
    <main className="ch-consent-page" aria-busy={busy || undefined}>
      <article className="ch-consent-card">
        <p className="ch-consent-brand">CallHarbor</p>
        <h1 ref={headingRef} tabIndex={-1}>
          {heading}
        </h1>
        {children}
      </article>
    </main>
  );
}
