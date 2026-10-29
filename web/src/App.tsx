import { useEffect, useRef, useState } from "react";
import { getHealth, search, streamChat, type Health, type SearchHit } from "./api";
import { applyEvent, historyFor, newAssistant, type AssistantMessage, type Message } from "./chat";
import { Answer } from "./components/Answer";

const examples = [
  { lang: "EN", text: "Is there data on air pollution monitoring?" },
  { lang: "RU", text: "Свободные земельные участки в Павлодарской области" },
  { lang: "KK", text: "Қарағанды облысындағы ауруханалар туралы деректер бар ма?" },
  { lang: "EN", text: "Unemployment statistics for Astana" },
];

export default function App() {
  const [health, setHealth] = useState<Health | null>(null);
  const [messages, setMessages] = useState<Message[]>([]);
  const [hits, setHits] = useState<SearchHit[] | null>(null);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    getHealth().then(setHealth).catch(() => setHealth(null));
  }, []);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [messages, hits]);

  const agentEnabled = health?.agent.enabled ?? true;

  async function ask(question: string) {
    question = question.trim();
    if (!question || busy) return;
    setInput("");
    setBusy(true);

    if (!agentEnabled) {
      try {
        setMessages([{ role: "user", content: question }]);
        setHits(await search(question));
      } finally {
        setBusy(false);
      }
      return;
    }

    const history = historyFor(messages, question);
    const index = messages.length + 1;
    setMessages((m) => [...m, { role: "user", content: question }, newAssistant()]);
    const update = (fn: (a: AssistantMessage) => AssistantMessage) =>
      setMessages((m) => m.map((msg, i) => (i === index && msg.role === "assistant" ? fn(msg) : msg)));

    const controller = new AbortController();
    abortRef.current = controller;
    try {
      await streamChat(history, (e) => update((a) => applyEvent(a, e)), controller.signal);
      update((a) => (a.status === "streaming" ? { ...a, status: "error", error: "The stream ended unexpectedly." } : a));
    } catch (err) {
      const aborted = controller.signal.aborted;
      update((a) => ({ ...a, status: "error", error: aborted ? "Stopped." : String((err as Error).message ?? err) }));
    } finally {
      abortRef.current = null;
      setBusy(false);
    }
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      ask(input);
    }
  }

  const empty = messages.length === 0;
  return (
    <div className="app">
      <header className="header">
        <div className="brand">
          <img src="/favicon.svg" alt="" width={28} height={28} />
          <div>
            <h1>Ask data.egov.kz</h1>
            <p className="tagline">AI agent over Kazakhstan’s open-data catalog</p>
          </div>
        </div>
        {health && (
          <div className={`pill${agentEnabled ? "" : " pill-off"}`} title={health.agent.reason ?? health.agent.effort}>
            {health.datasets?.toLocaleString()} datasets · {agentEnabled ? health.agent.model : "search-only"}
          </div>
        )}
        {empty || (
          <button
            className="ghost"
            onClick={() => {
              abortRef.current?.abort();
              setMessages([]);
              setHits(null);
            }}
          >
            New chat
          </button>
        )}
      </header>

      <main className="thread">
        {empty && (
          <section className="intro">
            <h2>Find the right government dataset by asking</h2>
            <p>
              The agent searches ~{(health?.datasets ?? 3900).toLocaleString()} dataset passports from{" "}
              <a href="https://data.egov.kz" target="_blank" rel="noreferrer">
                data.egov.kz
              </a>
              , checks the candidates and answers with links. Ask in English, Russian or Kazakh.
            </p>
            {!agentEnabled && (
              <div className="notice">
                The AI agent is off on this server (no Anthropic API key), so questions run as a plain full-text search.
              </div>
            )}
            <div className="examples">
              {examples.map((ex) => (
                <button key={ex.text} className="example" onClick={() => ask(ex.text)}>
                  <span className="lang">{ex.lang}</span>
                  {ex.text}
                </button>
              ))}
            </div>
          </section>
        )}

        {messages.map((m, i) =>
          m.role === "user" ? (
            <div key={i} className="user">
              {m.content}
            </div>
          ) : (
            <Answer key={i} msg={m} />
          ),
        )}

        {hits && (
          <ul className="hits">
            {hits.length === 0 && <li className="muted">No datasets matched.</li>}
            {hits.map((h) => (
              <li key={h.api_uri}>
                <a href={h.url} target="_blank" rel="noreferrer">
                  {h.title_ru}
                </a>
                <div className="source-meta">{[h.category_ru, h.agency_ru].filter(Boolean).join(" · ")}</div>
              </li>
            ))}
          </ul>
        )}
        <div ref={bottomRef} />
      </main>

      <form
        className="composer"
        onSubmit={(e) => {
          e.preventDefault();
          ask(input);
        }}
      >
        <textarea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Ask about any public dataset… (Enter to send)"
          rows={1}
          maxLength={4000}
          aria-label="Your question"
        />
        {busy && agentEnabled ? (
          <button type="button" className="send stop" onClick={() => abortRef.current?.abort()}>
            Stop
          </button>
        ) : (
          <button type="submit" className="send" disabled={!input.trim()}>
            Ask
          </button>
        )}
      </form>
    </div>
  );
}
