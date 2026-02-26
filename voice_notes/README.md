# Voice Notes Feature

## Overview
The PicoFlare Telegram bot supports voice notes. When you send a voice message:

1. **With `OPENROUTER_API_KEY`** — The audio is transcribed via OpenRouter (Gemini 2.5 Flash) and the text is passed to the agent. The agent responds to what you said.
2. **Without** — The voice is stored in R2 and the agent is told you sent a voice note (transcription disabled).

Voice transcription uses the same `OPENROUTER_API_KEY` as the LLM — no separate key needed.

### /voicenote — Save a voice message

Reply to a voice message with `/voicenote` to save it. The bot will:
1. Download and transcribe the audio (via OpenRouter)
2. Store the audio in R2 at `users/{id}/files/voice_{timestamp}.ogg`
3. Store the transcript in R2 at `users/{id}/notes/voice_{timestamp}.txt`
4. Reply with a confirmation and transcript preview

Without `/voicenote`, voice messages are transcribed and sent to the agent for a response. Use `/voicenote` when you want to save without a reply.

## How Telegram Voice Notes Work

### Format
- Telegram sends voice messages as **OGG Vorbis** audio files
- File extension: `.ogg`
- Content type: `audio/ogg` or `audio/voice` (Telegram-specific)

### Receiving Voice Messages

1. **Webhook Update**: Telegram sends a webhook payload containing the voice message metadata
2. **File Info**: The payload includes a `voice` object with:
   - `file_id`: Unique identifier for the voice file
   - `file_unique_id`: Unique ID across all bot installs
   - `duration`: Length of the voice message in seconds
   - `mime_type`: Usually `audio/ogg`
   - `file_size`: File size in bytes

3. **Download**: Use the `file_id` to call Telegram Bot API:
   ```
   GET https://api.telegram.org/bot<TOKEN>/getFile?file_id=<FILE_ID>
   ```
   This returns a `file_path` which is used to download the actual file.

4. **Storage**: Downloaded OGG files are stored in this folder with a naming convention like:
   - `{user_id}_{timestamp}_{file_id}.ogg`
   - Or hashed names for privacy

### Processing Pipeline

```
Telegram → Webhook → Download OGG → Store → (Optional: Transcribe to Text)
```

### Optional: Transcription
- OGG files can be converted to text using Whisper API or similar
- Transcription enables voice-to-text features and searchability

## File Structure

```
voice_notes/
├── README.md          # This file
├── {user_id}/         # User-specific folders (optional)
│   ├── 2024-01-15_10-30-00_abc123.ogg
│   └── 2024-01-15_11-45-00_def456.ogg
└── transcripts/       # Transcribed text (optional)
    └── abc123.txt
```

## Storage Notes

- OGG files are compressed and efficient for voice storage
- Typical voice note: 10-100KB per minute
- Consider implementing cleanup/retention policies for long-term storage