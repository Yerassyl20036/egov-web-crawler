// Types and transport for the Go API (see internal/api and internal/agent).

export interface DatasetRef {
  api_uri: string;
  title: string;
  category?: string;
  agency?: string;
  url: string;
}

export interface Citation extends DatasetRef {
  verified: boolean;
}

export interface AgentResult {
  answer: string;
  stop_reason: string;
  model: string;
  turns: number;
  citations: Citation[];
  usage: { input_tokens: number; output_tokens: number; cache_read_tokens: number };
  duration_ms: number;
}

export type AgentEvent =
  | { type: "thinking"; text: string }
  | { type: "text"; text: string }
  | { type: "tool_call"; id: string; name: string; input: Record<string, unknown> }
  | { type: "tool_result"; id: string; name: string; summary: string; datasets?: DatasetRef[]; is_error?: boolean }
  | { type: "fallback"; text: string }
  | { type: "refusal"; text: string }
  | { type: "done"; done: AgentResult }
  | { type: "error"; text: string; code?: string };

export interface ChatTurn {
  role: "user" | "assistant";
  content: string;
}

export interface Health {
  status: string;
  datasets?: number;
  preview_rows: boolean;
  agent: { enabled: boolean; model?: string; effort?: string; reason?: string };
}

export interface SearchHit extends DatasetRef {
  title_ru: string;
  description_ru?: string;
  category_ru?: string;
  agency_ru?: string;
  score: number;
}

export async function getHealth(): Promise<Health> {
  const res = await fetch("/api/health");
  return res.json();
}

export async function search(q: string): Promise<SearchHit[]> {
  const res = await fetch(`/api/search?q=${encodeURIComponent(q)}&limit=10`);
  if (!res.ok) throw new Error((await res.json()).error ?? res.statusText);
  const body = await res.json();
  return body.results;
}

/**
 * POSTs the conversation and parses the Server-Sent Events stream.
 * (EventSource can't POST, so this reads the body stream by hand.)
 */
export async function streamChat(
  messages: ChatTurn[],
  onEvent: (e: AgentEvent) => void,
  signal: AbortSignal,
): Promise<void> {
  const res = await fetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ messages }),
    signal,
  });
  if (!res.ok || !res.body) {
    let message = res.statusText;
    try {
      message = (await res.json()).error ?? message;
    } catch {
      /* not JSON */
    }
    throw new Error(message);
  }

  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += value;
    let sep: number;
    while ((sep = buffer.indexOf("\n\n")) >= 0) {
      const frame = buffer.slice(0, sep);
      buffer = buffer.slice(sep + 2);
      const data = frame
        .split("\n")
        .filter((line) => line.startsWith("data: "))
        .map((line) => line.slice(6))
        .join("\n");
      if (data) onEvent(JSON.parse(data) as AgentEvent);
    }
  }
}

const datasetLink = /data\.egov\.kz\/datasets\/view\?index=([A-Za-z0-9_-]+)/;

/** Returns the api_uri if href points at a dataset page on the portal. */
export function datasetIdFromHref(href: string | undefined): string | null {
  const m = href?.match(datasetLink);
  return m ? m[1] : null;
}
