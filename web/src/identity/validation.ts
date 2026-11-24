import { MAX_PASSWORD_LENGTH, MIN_PASSWORD_LENGTH, USERNAME_PATTERN } from "./api";

export interface FirstAdminErrors {
  readonly username?: string;
  readonly password?: string;
  readonly confirm?: string;
}

export function validateFirstAdmin(username: string, password: string, confirm: string): FirstAdminErrors {
  const errors: { username?: string; password?: string; confirm?: string } = {};
  if (!USERNAME_PATTERN.test(username)) {
    errors.username = "Use 1 to 64 letters, numbers, dots, underscores or hyphens.";
  }
  if (password.length < MIN_PASSWORD_LENGTH) {
    errors.password = `Password must be at least ${MIN_PASSWORD_LENGTH} characters.`;
  } else if (password.length > MAX_PASSWORD_LENGTH) {
    errors.password = `Password must be at most ${MAX_PASSWORD_LENGTH} characters.`;
  }
  if (confirm !== password) {
    errors.confirm = "Passwords do not match.";
  }
  return errors;
}

export function hasErrors(errors: FirstAdminErrors): boolean {
  return Object.values(errors).some((message) => message !== undefined);
}
