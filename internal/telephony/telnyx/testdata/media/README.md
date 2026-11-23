Telnyx media WebSocket fixtures. Shapes are transcribed from the official
media-streaming guide (https://developers.telnyx.com/docs/voice/programmable-voice/media-streaming,
retrieved 2026-11-23): connected l.117-122, start l.143-163, stop l.204-214,
mark l.347-356, dtmf l.364-374. The guide's error example is a type sketch,
not JSON (l.382-392); error.json instantiates it with documented code 100003.
Media frames are generated in tests from real PCMU audio because the guide's
example uses the placeholder track value "inbound/outbound".
