// Chat state: folds the agent's event stream into renderable messages.
import type { AgentEvent, AgentResult, ChatTurn, DatasetRef, SearchHit } from "./api";

export type Step =
  | {
      kind: "tool";
      id: string;
      name: string;
      input: Record<string, unknown>;
      summary?: string;
      datasets?: DatasetRef[];
      isError?: boolean;
      pending: boolean;
    }
  | { kind: "note"; text: string }
  | { kind: "fallback"; text: string };

export interface UserMessage {
  role: "user";
  content: string;
}

export interface AssistantMessage {
  role: "assistant";
  steps: Step[];
  answer: string;
  status: "streaming" | "done" | "error" | "refused";
  error?: string;
  /** "llm_quota": the model is out of quota; hits holds plain search results instead. */
  errorCode?: string;
  hits?: SearchHit[];
  result?: AgentResult;
}

export type Message = UserMessage | AssistantMessage;

export function newAssistant(): AssistantMessage {
  return { role: "assistant", steps: [], answer: "", status: "streaming" };
}

/** Pure reducer: returns a new assistant message with the event applied. */
export function applyEvent(msg: AssistantMessage, e: AgentEvent): AssistantMessage {
  const steps = [...msg.steps];
  switch (e.type) {
    case "thinking": {
      const last = steps[steps.length - 1];
      if (last?.kind === "note") steps[steps.length - 1] = { ...last, text: last.text + e.text };
      else steps.push({ kind: "note", text: e.text });
      return { ...msg, steps };
    }
    case "text":
      return { ...msg, answer: msg.answer + e.text };
    case "tool_call": {
      // Text written before a tool call is a progress note, not the answer.
      if (msg.answer.trim()) steps.push({ kind: "note", text: msg.answer.trim() });
      steps.push({ kind: "tool", id: e.id, name: e.name, input: e.input ?? {}, pending: true });
      return { ...msg, steps, answer: "" };
    }
    case "tool_result":
      return {
        ...msg,
        steps: steps.map((s) =>
          s.kind === "tool" && s.id === e.id
            ? { ...s, summary: e.summary, datasets: e.datasets, isError: e.is_error, pending: false }
            : s,
        ),
      };
    case "fallback":
      steps.push({ kind: "fallback", text: e.text });
      return { ...msg, steps };
    case "refusal":
      // A refused stream may carry partial text; discard it.
      return { ...msg, answer: "", status: "refused", error: e.text };
    case "done":
      return { ...msg, status: msg.status === "refused" ? "refused" : "done", result: e.done };
    case "error":
      return { ...msg, status: "error", error: e.text, errorCode: e.code };
  }
}

/** The history sent to the API: completed question/answer pairs only. */
export function historyFor(messages: Message[], question: string): ChatTurn[] {
  const turns: ChatTurn[] = [];
  for (let i = 0; i + 1 < messages.length; i += 2) {
    const q = messages[i];
    const a = messages[i + 1];
    if (q.role === "user" && a.role === "assistant" && a.status === "done" && a.answer.trim()) {
      turns.push({ role: "user", content: q.content }, { role: "assistant", content: a.answer });
    }
  }
  turns.push({ role: "user", content: question });
  return turns.slice(-19);
}
