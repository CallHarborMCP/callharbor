import { useState, type FormEvent } from "react";
import { ApiError, createFirstAdmin } from "./api";
import { hasErrors, validateFirstAdmin, type FirstAdminErrors } from "./validation";

export interface FirstAdminWizardProps {
  readonly onCreated: () => void;
}

const serverMessages: Record<string, string> = {
  setup_complete: "Setup is already complete. Sign in with your existing account.",
  username_taken: "That username is already taken.",
  invalid_input: "The username or password was rejected. Check the requirements and try again.",
  internal: "Something went wrong while creating the account. Try again.",
  network: "The server could not be reached. Check your connection and try again.",
};

export function FirstAdminWizard({ onCreated }: FirstAdminWizardProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [errors, setErrors] = useState<FirstAdminErrors>({});
  const [serverError, setServerError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validateFirstAdmin(username, password, confirm);
    setErrors(found);
    setServerError(null);
    if (hasErrors(found)) {
      return;
    }
    setSubmitting(true);
    try {
      await createFirstAdmin({ username, password });
      setPassword("");
      setConfirm("");
      onCreated();
    } catch (error) {
      const code = error instanceof ApiError ? error.code : "internal";
      setServerError(serverMessages[code] ?? serverMessages.internal);
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <section aria-labelledby="first-admin-heading">
      <h1 id="first-admin-heading">Create the first administrator</h1>
      <p>This account manages users and has full administrative access to CallHarbor.</p>
      <form onSubmit={handleSubmit} noValidate>
        <div>
          <label htmlFor="admin-username">Username</label>
          <input
            id="admin-username"
            name="username"
            autoComplete="username"
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            aria-invalid={errors.username ? true : undefined}
            aria-describedby={errors.username ? "admin-username-error" : undefined}
          />
          {errors.username ? <p id="admin-username-error" role="alert">{errors.username}</p> : null}
        </div>
        <div>
          <label htmlFor="admin-password">Password</label>
          <input
            id="admin-password"
            name="password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            aria-invalid={errors.password ? true : undefined}
            aria-describedby={errors.password ? "admin-password-error" : undefined}
          />
          {errors.password ? <p id="admin-password-error" role="alert">{errors.password}</p> : null}
        </div>
        <div>
          <label htmlFor="admin-confirm">Confirm password</label>
          <input
            id="admin-confirm"
            name="confirm"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(event) => setConfirm(event.target.value)}
            aria-invalid={errors.confirm ? true : undefined}
            aria-describedby={errors.confirm ? "admin-confirm-error" : undefined}
          />
          {errors.confirm ? <p id="admin-confirm-error" role="alert">{errors.confirm}</p> : null}
        </div>
        {serverError ? <p role="alert">{serverError}</p> : null}
        <button type="submit" disabled={submitting}>
          {submitting ? "Creating account..." : "Create administrator"}
        </button>
      </form>
    </section>
  );
}
