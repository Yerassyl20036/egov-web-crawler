import type { Step } from "../chat";

const toolLabels: Record<string, string> = {
  search_datasets: "Searched the catalog",
  get_dataset: "Read dataset passport",
  list_categories: "Listed categories",
  preview_rows: "Previewed rows",
};

function describeInput(name: string, input: Record<string, unknown>): string {
  if (name === "search_datasets") {
    const filters = ["category", "agency"]
      .filter((k) => input[k])
      .map((k) => `${k}: ${String(input[k])}`);
    return [String(input.query ?? ""), ...filters].filter(Boolean).join(" · ");
  }
  if (typeof input.api_uri === "string") return input.api_uri;
  return "";
}

export function Steps({ steps, streaming }: { steps: Step[]; streaming: boolean }) {
  if (steps.length === 0) return null;
  const toolCount = steps.filter((s) => s.kind === "tool").length;
  return (
    <details className="steps" open={streaming}>
      <summary>
        {streaming ? "Working…" : "Agent steps"}
        <span className="steps-count">
          {toolCount} tool call{toolCount === 1 ? "" : "s"}
        </span>
      </summary>
      <ol>
        {steps.map((step, i) => {
          if (step.kind === "note") {
            return (
              <li key={i} className="step step-note">
                {step.text}
              </li>
            );
          }
          if (step.kind === "fallback") {
            return (
              <li key={i} className="step step-fallback">
                Refusal fallback: {step.text}
              </li>
            );
          }
          return (
            <li key={step.id} className={`step step-tool${step.isError ? " step-error" : ""}`}>
              <div className="step-head">
                <span className={`dot${step.pending ? " dot-pending" : ""}`} aria-hidden />
                <span className="step-name">{toolLabels[step.name] ?? step.name}</span>
                <code className="step-input">{describeInput(step.name, step.input)}</code>
              </div>
              {step.summary && <div className="step-summary">→ {step.summary}</div>}
              {step.datasets && step.datasets.length > 0 && step.name === "search_datasets" && (
                <ul className="step-datasets">
                  {step.datasets.slice(0, 5).map((d) => (
                    <li key={d.api_uri}>
                      <a href={d.url} target="_blank" rel="noreferrer">
                        {d.title}
                      </a>
                    </li>
                  ))}
                  {step.datasets.length > 5 && <li className="muted">+{step.datasets.length - 5} more</li>}
                </ul>
              )}
            </li>
          );
        })}
      </ol>
    </details>
  );
}
