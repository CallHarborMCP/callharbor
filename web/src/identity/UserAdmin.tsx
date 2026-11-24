import { useEffect, useState, type FormEvent } from "react";
import { MAX_PASSWORD_LENGTH, MIN_PASSWORD_LENGTH, USERNAME_PATTERN } from "./api";
import {
  createUser,
  listUsers,
  resetPassword,
  updateUser,
  UserAdminError,
  type ManagedUser,
  type Role,
} from "./users";

export interface UserAdminProps {
  readonly csrfToken: string;
  readonly currentUserId: string;
}

const messages: Record<string, string> = {
  username_taken: "That username is already taken.",
  invalid_input: "The username, password or role was rejected. Check the requirements and try again.",
  last_admin: "At least one active administrator must remain.",
  forbidden: "You are not allowed to make that change.",
  not_found: "That user no longer exists. Refresh the list.",
  network: "The server could not be reached. Check your connection and try again.",
  internal: "Something went wrong. Try again.",
};

function describe(error: unknown): string {
  const code = error instanceof UserAdminError ? error.code : "internal";
  return messages[code] ?? messages.internal;
}

export function UserAdmin({ csrfToken, currentUserId }: UserAdminProps) {
  const [users, setUsers] = useState<ManagedUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let active = true;
    listUsers()
      .then((loaded) => {
        if (!active) return;
        setUsers(loaded);
        setError(null);
      })
      .catch((caught: unknown) => {
        if (active) setError(describe(caught));
      });
    return () => {
      active = false;
    };
  }, [reloadKey]);

  async function run(action: () => Promise<void>, success: string) {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await action();
      setNotice(success);
      setReloadKey((key) => key + 1);
    } catch (caught) {
      setError(describe(caught));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section aria-labelledby="users-heading">
      <h2 id="users-heading">Users</h2>
      {error ? <p role="alert">{error}</p> : null}
      {notice ? <p role="status">{notice}</p> : null}
      {users === null ? (
        <p aria-busy="true">Loading users...</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th scope="col">Username</th>
              <th scope="col">Role</th>
              <th scope="col">Status</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {users.map((user) => (
              <UserRow
                key={user.id}
                user={user}
                isSelf={user.id === currentUserId}
                disabled={busy}
                onChangeRole={(role) =>
                  run(() => updateUser(csrfToken, user.id, { role }), `Role updated for ${user.username}.`)
                }
                onToggleActive={() =>
                  run(
                    () => updateUser(csrfToken, user.id, { active: !user.active }),
                    `${user.username} ${user.active ? "deactivated" : "activated"}.`,
                  )
                }
                onResetPassword={(password) =>
                  run(() => resetPassword(csrfToken, user.id, password), `Password reset for ${user.username}. Their sessions were revoked.`)
                }
              />
            ))}
          </tbody>
        </table>
      )}
      <CreateUserForm
        disabled={busy}
        onCreate={(input) =>
          run(async () => {
            await createUser(csrfToken, input);
          }, `Created user ${input.username}.`)
        }
      />
    </section>
  );
}

interface UserRowProps {
  readonly user: ManagedUser;
  readonly isSelf: boolean;
  readonly disabled: boolean;
  readonly onChangeRole: (role: Role) => void;
  readonly onToggleActive: () => void;
  readonly onResetPassword: (password: string) => void;
}

function UserRow({ user, isSelf, disabled, onChangeRole, onToggleActive, onResetPassword }: UserRowProps) {
  const [password, setPassword] = useState("");
  const [passwordError, setPasswordError] = useState<string | null>(null);
  const nextRole: Role = user.role === "admin" ? "user" : "admin";

  function handleReset(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (password.length < MIN_PASSWORD_LENGTH || password.length > MAX_PASSWORD_LENGTH) {
      setPasswordError(`Password must be ${MIN_PASSWORD_LENGTH} to ${MAX_PASSWORD_LENGTH} characters.`);
      return;
    }
    setPasswordError(null);
    onResetPassword(password);
    setPassword("");
  }

  return (
    <tr>
      <td>{user.username}{isSelf ? " (you)" : ""}</td>
      <td>{user.role}</td>
      <td>{user.active ? "Active" : "Deactivated"}</td>
      <td>
        <button type="button" disabled={disabled || isSelf} onClick={() => onChangeRole(nextRole)}>
          {user.role === "admin" ? `Make ${user.username} a user` : `Make ${user.username} an admin`}
        </button>{" "}
        <button type="button" disabled={disabled || isSelf} onClick={onToggleActive}>
          {user.active ? `Deactivate ${user.username}` : `Activate ${user.username}`}
        </button>
        <form onSubmit={handleReset} noValidate>
          <label htmlFor={`reset-${user.id}`}>New password for {user.username}</label>
          <input
            id={`reset-${user.id}`}
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            aria-describedby={passwordError ? `reset-${user.id}-error` : undefined}
          />
          <button type="submit" disabled={disabled}>Reset password</button>
          {passwordError ? <p id={`reset-${user.id}-error`} role="alert">{passwordError}</p> : null}
        </form>
      </td>
    </tr>
  );
}

interface CreateUserFormProps {
  readonly disabled: boolean;
  readonly onCreate: (input: { username: string; password: string; role: Role }) => void;
}

function CreateUserForm({ disabled, onCreate }: CreateUserFormProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("user");
  const [error, setError] = useState<string | null>(null);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!USERNAME_PATTERN.test(username)) {
      setError("Use 1 to 64 letters, numbers, dots, underscores or hyphens.");
      return;
    }
    if (password.length < MIN_PASSWORD_LENGTH || password.length > MAX_PASSWORD_LENGTH) {
      setError(`Password must be ${MIN_PASSWORD_LENGTH} to ${MAX_PASSWORD_LENGTH} characters.`);
      return;
    }
    setError(null);
    onCreate({ username, password, role });
    setUsername("");
    setPassword("");
  }

  return (
    <section aria-labelledby="create-user-heading">
      <h3 id="create-user-heading">Create user</h3>
      <form onSubmit={handleSubmit} noValidate>
        <div>
          <label htmlFor="new-username">Username</label>
          <input id="new-username" autoComplete="off" value={username} onChange={(event) => setUsername(event.target.value)} />
        </div>
        <div>
          <label htmlFor="new-password">Initial password</label>
          <input
            id="new-password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </div>
        <div>
          <label htmlFor="new-role">Role</label>
          <select id="new-role" value={role} onChange={(event) => setRole(event.target.value as Role)}>
            <option value="user">User</option>
            <option value="admin">Administrator</option>
          </select>
        </div>
        {error ? <p role="alert">{error}</p> : null}
        <button type="submit" disabled={disabled}>Create user</button>
      </form>
    </section>
  );
}
