# Agent Prompts: Switch Voice Transcription from OpenAI to OpenRouter

Copy and paste these prompts into your PicoFlare Telegram bot. The agent will use Code Mode (read_file, write_file, edit_file) to make the changes.

---

## Prompt 1: Main Task (paste this first)

```
Switch voice note transcription from OpenAI Whisper to OpenRouter. 

Current flow: pkg/transcribe/whisper.go calls OpenAI's /v1/audio/transcriptions API. The bot uses OPENAI_API_KEY in pkg/bot/telegram.go.

New flow: Use OpenRouter's chat completions API with audio input. OpenRouter accepts base64-encoded audio in the message content with type "input_audio". Format: {"type": "input_audio", "input_audio": {"data": "<base64>", "format": "ogg"}}. Use model "openai/gpt-4o-audio-preview" (or check openrouter.ai/models for audio-capable models). The API key is OPENROUTER_API_KEY (same as the LLM).

Tasks:
1. Create pkg/transcribe/openrouter.go - Transcribe(ctx, apiKey, audioData, filename) that POSTs to https://openrouter.ai/api/v1/chat/completions with messages: [{"role":"user","content":[{"type":"text","text":"Transcribe this audio. Return only the transcribed text, nothing else."},{"type":"input_audio","input_audio":{"data":"<base64>","format":"ogg"}}]}]. Parse the response and return the text.
2. Update pkg/bot/telegram.go: Replace transcribe.Transcribe with the OpenRouter version. Use the bot's OpenRouter API key (we need to pass it - it's LLMAPIKey in config). Remove the openAIApiKey / OpenAIApiKey usage for voice. Voice transcription should use OPENROUTER_API_KEY instead.
3. Update main.go: Remove OpenAIApiKey from bot config for voice (or keep it unused). Voice will use the same key as the LLM.
4. Update .env.example: Remove or comment OPENAI_API_KEY for voice. Add note that voice uses OPENROUTER_API_KEY.
5. Update voice_notes/README.md to say we use OpenRouter for transcription.

Use read_file to inspect the current code, then write_file or edit_file to make changes. Run "go build ./..." after to verify.
```

---

## Prompt 2: If the agent needs more context

```
For the OpenRouter audio transcription in pkg/transcribe/openrouter.go:

- Endpoint: POST https://openrouter.ai/api/v1/chat/completions
- Headers: Authorization: Bearer <OPENROUTER_API_KEY>, Content-Type: application/json
- Body: {"model": "openai/gpt-4o-audio-preview", "messages": [{"role": "user", "content": [{"type": "text", "text": "Transcribe this audio. Return only the transcribed text."}, {"type": "input_audio", "input_audio": {"data": "<base64 of audio bytes>", "format": "ogg"}}]}]}
- Response: standard chat completion, choices[0].message.content has the transcription
- Telegram voice notes are OGG Vorbis, so format "ogg" should work. If not, try "webm" or convert to wav.
```

---

## Prompt 3: Bot wiring (if agent is confused)

```
In pkg/bot/telegram.go, the bot needs the OpenRouter API key for voice transcription. Currently it has b.openAIApiKey. Change it to use the LLM API key instead: add a field like openRouterApiKey or reuse the agent's key. The bot config already has LLMAPIKey (OpenRouter). Pass that to the bot and use it for transcribe.Transcribe when handling voice. So: voice transcription uses OPENROUTER_API_KEY (same as LLM), not OPENAI_API_KEY.
```

---

## Quick one-liner (minimal)

```
Change voice transcription from OpenAI Whisper to OpenRouter. Create pkg/transcribe/openrouter.go that POSTs base64 audio to OpenRouter chat completions with input_audio. Use model openai/gpt-4o-audio-preview. Update the bot to use OPENROUTER_API_KEY for voice instead of OPENAI_API_KEY.
```
