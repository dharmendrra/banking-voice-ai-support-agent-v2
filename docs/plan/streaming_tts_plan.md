# True End-to-End Streaming & Chunked TTS Plan

This plan describes how we stream text chunks from the LLM, group them into sentences dynamically on the client, and feed them into the TTS player while preserving barge-in and conversational resume states.

---

## 1. Architectural Architecture Flow

```mermaid
graph TD
    Orchestrator["Ollama LLM (Streaming Tokens)"] -->|ND-JSON Chunks| ME["media-engine"]
    ME -->|WebSocket text_chunk| Browser["Browser WebSocket Client"]
    
    subgraph Browser Client (frontend/index.html)
        Parser["Sentence Parser (Accumulator)"] -->|Sentence Completed| Queue[("Audio Playback Queue (audioQueue)")]
        Queue -->|Shift & Play| Play["TTS Synthesis (Kokoro / Native)"]
        
        BargeIn["User Speech Detected (Barge-in)"] -->|Halt Audio| Play
        BargeIn -->|Preserve Backlog| Queue
    end
```

---

## 2. Technical Implementations

### A. Streaming Text Chunks from media-engine
The `media-engine` already receives streaming text chunks from the orchestrator. Currently, it just forwards them directly to the client WebSocket:
* `media-engine` writes chunks of type `speech` directly to the client:
  ```json
  {"type": "speech", "text": "Hello"}
  {"type": "speech", "text": " Dharmendra"}
  ```

### B. Client-Side On-the-Fly Sentence Builder (`index.html`)
Instead of waiting for the `final` message type over the WebSocket, the client browser will accumulate incoming `speech` chunks and split them into sentences as soon as punctuation is detected.

**JavaScript Implementation Template**:
```javascript
let streamTextBuffer = ""; // Accumulates incoming token text
let currentlyPlayingPlayId = 0;

function handleIncomingSpeechChunk(textChunk) {
    streamTextBuffer += textChunk;
    
    // Split the current buffer by sentence terminators (. ! ? \n)
    // We keep the terminator attached to the sentence
    const sentenceEndingsRegex = /[^.!?\n]+[.!?\n]+/g;
    let match;
    let lastIndex = 0;
    
    while ((match = sentenceEndingsRegex.exec(streamTextBuffer)) !== null) {
        const sentence = match[0].trim();
        if (sentence.length > 0) {
            enqueueSentence(sentence);
        }
        lastIndex = sentenceEndingsRegex.lastIndex;
    }
    
    // Keep the remaining incomplete sentence in the buffer
    streamTextBuffer = streamTextBuffer.substring(lastIndex);
}

function enqueueSentence(sentence) {
    audioQueue.push({
        text: sentence,
        playId: currentlyPlayingPlayId
    });
    
    // If the player is currently idle, start playing the first sentence immediately
    if (!isPlayingQueue) {
        processAudioQueue();
    }
}
```

---

## 3. Handling Barge-in & Conversational Resume

If the customer speaks while the agent is speaking, we must halt the playback immediately (barge-in) but preserve the remaining queue contents so the user can say "resume" or "go on".

### A. Interruption Handler (Barge-in)
When speech recognition (`onstart` or `onresult` with confidence) detects that the user is talking:
1. **Stop Audio Playback**: Immediately pause `activeAudio` or call `speechSynthesis.cancel()`.
2. **Preserve Backlog**: Keep the remaining items in `audioQueue` (do **not** empty it).
3. **Save History Context**: Store the queue state under a `suspendedQueue` variable so we can reclaim it.

```javascript
function handleUserInterruption() {
    if (isTTSPlaying || activeAudio !== null) {
        console.log("[Barge-in] User interrupted agent. Pausing audio queue.");
        
        // Pause active audio stream
        if (activeAudio) {
            activeAudio.pause();
        }
        if ('speechSynthesis' in window) {
            window.speechSynthesis.cancel();
        }
        
        // Save remaining queue for potential resume
        suspendedQueue = [...audioQueue];
        isTTSPlaying = false;
        isPlayingQueue = false;
    }
}
```

### B. Resume Playback Handler ("go on" / "resume")
If the user says a resume command keyword (like "go on", "resume", or "continue"):
1. The orchestrator detects the path type as `resume_playback`.
2. The client receives the `resume_playback` message type over the WebSocket:
3. Instead of playing new LLM text, the client restores the `suspendedQueue` back into the `audioQueue` and starts playing:

```javascript
case "resume_playback":
    if (suspendedQueue && suspendedQueue.length > 0) {
        console.log("[Resume] Restoring audio queue and resuming playback.");
        audioQueue = [...suspendedQueue];
        suspendedQueue = [];
        processAudioQueue();
    } else {
        // Fallback: if no queue was suspended, say a standard acknowledgment
        speakTTS("Sure, continuing where we left off.");
    }
    break;
```
