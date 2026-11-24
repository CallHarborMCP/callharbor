import { useState, type FormEvent } from "react";
import {
  AIProfileError,
  createAIProfile,
  MAX_INSTRUCTIONS,
  PROTOCOL_LABELS,
  validateEndpoint,
  type AIProtocol,
} from "./profiles";

export interface AIProfileFormProps {
  readonly csrfToken: string;
  readonly onCreated: (name: string) => void;
}

const messages: Record<string, string> = {
  invalid_input: "The profile was rejected. Check the endpoint, model, voice and instructions.",
  unsupported_media_type: "The request could not be sent. Reload and try again.",
  network: "The server could not be reached. Check your connection and try again.",
  internal: "Something went wrong. Try again.",
};

function describe(error: unknown): string {
  const code = error instanceof AIProfileError ? error.code : "internal";
  return messages[code] ?? messages.internal;
}

export function AIProfileForm({ csrfToken, onCreated }: AIProfileFormProps) {
  const [name, setName] = useState("");
  const [protocol, setProtocol] = useState<AIProtocol>("realtime_ws");
  const [endpoint, setEndpoint] = useState("");
  const [model, setModel] = useState("");
  const [voice, setVoice] = useState("");
  const [instructions, setInstructions] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const endpointError = validateEndpoint(endpoint.trim());
    if (name.trim() === "") {
      setError("Enter a profile name.");
      return;
    }
    if (endpointError) {
      setError(endpointError);
      return;
    }
    if (model.trim() === "" || voice.trim() === "") {
      setError("Model and voice are required.");
      return;
    }
    if (instructions.length > MAX_INSTRUCTIONS) {
      setError(`Instructions must be at most ${MAX_INSTRUCTIONS} characters.`);
      return;
    }
    setError(null);
    setBusy(true);
    try {
      await createAIProfile(csrfToken, {
        name: name.trim(),
        protocol,
        endpoint_url: endpoint.trim(),
        model: model.trim(),
        voice: voice.trim(),
        instructions,
      });
      onCreated(name.trim());
      setName("");
      setEndpoint("");
      setModel("");
      setVoice("");
      setInstructions("");
    } catch (caught) {
      setError(describe(caught));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section aria-labelledby="ai-profile-heading">
      <h2 id="ai-profile-heading">AI voice profile</h2>
      <form onSubmit={handleSubmit} noValidate>
        <div>
          <label htmlFor="profile-name">Name</label>
          <input id="profile-name" autoComplete="off" value={name} onChange={(event) => setName(event.target.value)} />
        </div>
        <div>
          <label htmlFor="profile-protocol">Protocol</label>
          <select
            id="profile-protocol"
            value={protocol}
            onChange={(event) => setProtocol(event.target.value as AIProtocol)}
          >
            {(Object.keys(PROTOCOL_LABELS) as AIProtocol[]).map((value) => (
              <option key={value} value={value}>
                {PROTOCOL_LABELS[value]}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label htmlFor="profile-endpoint">Endpoint URL</label>
          <input
            id="profile-endpoint"
            type="url"
            autoComplete="off"
            placeholder="wss://"
            value={endpoint}
            onChange={(event) => setEndpoint(event.target.value)}
          />
        </div>
        <div>
          <label htmlFor="profile-model">Model</label>
          <input id="profile-model" autoComplete="off" value={model} onChange={(event) => setModel(event.target.value)} />
        </div>
        <div>
          <label htmlFor="profile-voice">Voice</label>
          <input id="profile-voice" autoComplete="off" value={voice} onChange={(event) => setVoice(event.target.value)} />
        </div>
        <div>
          <label htmlFor="profile-instructions">Instructions</label>
          <textarea
            id="profile-instructions"
            value={instructions}
            onChange={(event) => setInstructions(event.target.value)}
          />
        </div>
        {error ? <p role="alert">{error}</p> : null}
        <button type="submit" disabled={busy}>
          {busy ? "Saving..." : "Save profile"}
        </button>
      </form>
    </section>
  );
}
