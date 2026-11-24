export type CallStatus = "ended" | "active" | "ringing" | "failed";
export type TaskOutcome = "success" | "partial" | "failed" | "unknown";

export interface TranscriptLine {
  readonly at: string;
  readonly speaker: "Assistant" | "Recipient";
  readonly text: string;
}

export interface TimelineEvent {
  readonly at: string;
  readonly label: string;
}

export interface CallRecord {
  readonly id: string;
  readonly to: string;
  readonly recipient: string;
  readonly objective: string;
  readonly status: CallStatus;
  readonly outcome: TaskOutcome;
  readonly startedAt: string;
  readonly duration: string;
  readonly protocol: "Realtime (WebSocket)" | "GPT-Live (WebSocket)";
  readonly recorded: boolean;
  readonly transcript: readonly TranscriptLine[];
  readonly timeline: readonly TimelineEvent[];
}

export const calls: readonly CallRecord[] = [
  {
    id: "call_7Q2K9",
    to: "+420 555 010 214",
    recipient: "Dental office, front desk",
    objective: "Move Thursday cleaning to the following week",
    status: "ended",
    outcome: "success",
    startedAt: "Nov 22, 2026 · 14:05",
    duration: "2 min 41 s",
    protocol: "Realtime (WebSocket)",
    recorded: true,
    transcript: [
      { at: "00:00:03", speaker: "Assistant", text: "Hello, this is an assistant calling on behalf of Tomas about a cleaning appointment." },
      { at: "00:00:11", speaker: "Recipient", text: "Sure, which day were you looking at?" },
      { at: "00:00:15", speaker: "Assistant", text: "Thursday the 26th at 9:30. Could we move it to the following week?" },
      { at: "00:00:24", speaker: "Recipient", text: "Next Thursday at 9:30 is open. I can book that." },
      { at: "00:00:31", speaker: "Assistant", text: "Perfect, thank you. Please confirm the new time by text." },
    ],
    timeline: [
      { at: "14:05:00", label: "Call queued" },
      { at: "14:05:02", label: "Dialing started" },
      { at: "14:05:09", label: "Recipient answered" },
      { at: "14:07:41", label: "Call ended by assistant" },
    ],
  },
  {
    id: "call_4LX83",
    to: "+420 555 010 877",
    recipient: "Parcel courier support",
    objective: "Ask for redelivery to the neighbour's address",
    status: "failed",
    outcome: "unknown",
    startedAt: "Nov 22, 2026 · 11:48",
    duration: "0 min 19 s",
    protocol: "GPT-Live (WebSocket)",
    recorded: false,
    transcript: [
      { at: "00:00:04", speaker: "Assistant", text: "Hi, I am calling about a parcel delivery." },
    ],
    timeline: [
      { at: "11:48:00", label: "Call queued" },
      { at: "11:48:03", label: "Dialing started" },
      { at: "11:48:22", label: "Media stream lost" },
    ],
  },
  {
    id: "call_9TR17",
    to: "+420 555 010 402",
    recipient: "Hairdresser salon",
    objective: "Book a trim on Saturday afternoon",
    status: "ended",
    outcome: "partial",
    startedAt: "Nov 21, 2026 · 17:22",
    duration: "4 min 10 s",
    protocol: "GPT-Live (WebSocket)",
    recorded: true,
    transcript: [
      { at: "00:00:05", speaker: "Assistant", text: "Hello, I would like to book a trim for Saturday afternoon." },
      { at: "00:00:14", speaker: "Recipient", text: "We have 2 pm or 4 pm, but the stylist is off after 3." },
    ],
    timeline: [
      { at: "17:22:00", label: "Call queued" },
      { at: "17:22:06", label: "Recipient answered" },
      { at: "17:26:10", label: "Call ended by recipient" },
    ],
  },
];

export const navItems = ["Dashboard", "Calls", "Profiles", "Settings"] as const;
