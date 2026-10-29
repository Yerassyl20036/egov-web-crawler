import Markdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { datasetIdFromHref, type Citation } from "../api";
import type { AssistantMessage } from "../chat";
import { Steps } from "./Steps";

export function Answer({ msg }: { msg: AssistantMessage }) {
  const citations = msg.result?.citations ?? [];
  const byId = new Map(citations.map((c) => [c.api_uri, c]));

  // Dataset links render as citation chips; links the agent made up (not in
  // the catalog) are flagged once the answer is verified.
  const components: Components = {
    a({ href, children }) {
      const id = datasetIdFromHref(href);
      if (!id) {
        return (
          <a href={href} target="_blank" rel="noreferrer">
            {children}
          </a>
        );
      }
      const c = byId.get(id);
      const unverified = msg.status === "done" && c && !c.verified;
      return (
        <a
          className={`cite${unverified ? " cite-unverified" : ""}`}
          href={href}
          target="_blank"
          rel="noreferrer"
          title={unverified ? "Not found in the catalog" : c?.agency ?? id}
        >
          {children}
        </a>
      );
    },
  };

  const streaming = msg.status === "streaming";
  return (
    <div className="assistant">
      <Steps steps={msg.steps} streaming={streaming} />
      {msg.answer && (
        <div className="answer">
          <Markdown remarkPlugins={[remarkGfm]} components={components}>
            {msg.answer}
          </Markdown>
          {streaming && <span className="caret" aria-hidden />}
        </div>
      )}
      {streaming && !msg.answer && msg.steps.length === 0 && <div className="typing">Thinking…</div>}
      {msg.status === "refused" && <div className="notice notice-warn">{msg.error}</div>}
      {msg.status === "error" && <div className="notice notice-error">{msg.error}</div>}
      {msg.status === "done" && citations.length > 0 && <Sources citations={citations} />}
      {msg.result && (
        <div className="meta">
          {msg.result.model} · {msg.result.turns} step{msg.result.turns === 1 ? "" : "s"} ·{" "}
          {(msg.result.duration_ms / 1000).toFixed(1)}s ·{" "}
          {(msg.result.usage.input_tokens + msg.result.usage.output_tokens).toLocaleString()} tokens
        </div>
      )}
    </div>
  );
}

function Sources({ citations }: { citations: Citation[] }) {
  return (
    <div className="sources">
      <div className="sources-title">Sources</div>
      <ul>
        {citations.map((c, i) => (
          <li key={c.api_uri} className={c.verified ? "" : "source-unverified"}>
            <span className="source-index">{i + 1}</span>
            <div>
              <a href={c.url} target="_blank" rel="noreferrer">
                {c.title || c.api_uri}
              </a>
              <div className="source-meta">
                {c.verified ? [c.category, c.agency].filter(Boolean).join(" · ") : "Not found in the catalog — the link may be wrong"}
              </div>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
