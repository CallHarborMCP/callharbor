import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AIProfileForm } from "./AIProfileForm";
import { validateEndpoint } from "./profiles";

function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

describe("validateEndpoint", () => {
  it("accepts wss and loopback ws, rejects other ws and schemes", () => {
    expect(validateEndpoint("wss://api.example.com/v1/realtime")).toBeNull();
    expect(validateEndpoint("ws://127.0.0.1:9000/x")).toBeNull();
    expect(validateEndpoint("ws://api.example.com/x")).toMatch(/loopback/);
    expect(validateEndpoint("https://api.example.com")).toMatch(/wss/);
    expect(validateEndpoint("not a url")).toMatch(/valid/);
  });
});

describe("AIProfileForm", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("defaults the protocol explicitly and offers both primary adapters", () => {
    render(<AIProfileForm csrfToken="csrf" onCreated={() => {}} />);

    const select = screen.getByLabelText("Protocol") as HTMLSelectElement;
    expect(select.value).toBe("realtime_ws");
    const options = Array.from(select.options).map((option) => option.value);
    expect(options).toEqual(["realtime_ws", "live_ws"]);
  });

  it("posts the chosen protocol with CSRF and reports success", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("{}", { status: 201 }));
    vi.stubGlobal("fetch", fetchMock);
    const onCreated = vi.fn();
    render(<AIProfileForm csrfToken="csrf-1" onCreated={onCreated} />);

    fill("Name", "Front desk");
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "live_ws" } });
    fill("Endpoint URL", "wss://api.example.com/live");
    fill("Model", "gpt-live");
    fill("Voice", "alloy");
    fireEvent.click(screen.getByRole("button", { name: "Save profile" }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith("Front desk"));
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/ai-profiles",
      expect.objectContaining({
        method: "POST",
        headers: expect.objectContaining({ "X-CSRF-Token": "csrf-1" }),
      }),
    );
    const body = JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body)) as { protocol: string };
    expect(body.protocol).toBe("live_ws");
  });

  it("blocks plain ws to a remote host before sending", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    render(<AIProfileForm csrfToken="csrf" onCreated={() => {}} />);

    fill("Name", "Remote");
    fill("Endpoint URL", "ws://api.example.com/x");
    fill("Model", "m");
    fill("Voice", "v");
    fireEvent.click(screen.getByRole("button", { name: "Save profile" }));

    expect(screen.getByRole("alert").textContent).toMatch(/loopback/);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
