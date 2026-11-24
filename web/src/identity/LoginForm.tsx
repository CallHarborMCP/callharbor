import { useState, type FormEvent } from "react";
import { login, SessionError, type Session } from "./session";

export interface LoginFormProps {
  readonly onSignedIn: (session: Session) => void;
}

const messages: Record<string, string> = {
  invalid_credentials: "Invalid username or password.",
  throttled: "Too many failed sign-in attempts. Wait 15 minutes and try again.",
  network: "The server could not be reached. Check your connection and try again.",
  internal: "Something went wrong while signing in. Try again.",
};

export function LoginForm({ onSignedIn }: LoginFormProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (username.length === 0 || password.length === 0) {
      setError("Enter your username and password.");
      return;
    }
    setError(null);
    setSubmitting(true);
    try {
      const session = await login(username, password);
      setPassword("");
      onSignedIn(session);
    } catch (caught) {
      const code = caught instanceof SessionError ? caught.code : "internal";
      setError(messages[code] ?? messages.internal);
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <section aria-labelledby="sign-in-heading">
      <h1 id="sign-in-heading">Sign in to CallHarbor</h1>
      <form onSubmit={handleSubmit} noValidate>
        <div>
          <label htmlFor="login-username">Username</label>
          <input
            id="login-username"
            name="username"
            autoComplete="username"
            value={username}
            onChange={(event) => setUsername(event.target.value)}
          />
        </div>
        <div>
          <label htmlFor="login-password">Password</label>
          <input
            id="login-password"
            name="password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </div>
        {error ? <p role="alert">{error}</p> : null}
        <button type="submit" disabled={submitting}>
          {submitting ? "Signing in..." : "Sign in"}
        </button>
      </form>
    </section>
  );
}
