// Workflow execution engine — a TypeScript (Deno) port of
// ../../common/workflow.go. Executes GitHub Actions workflows: evaluates
// ${{ }} expressions via Deno, runs `run` steps and `uses` actions
// (node / composite / local / remote), and collects outputs and annotations.
//
// Semantics track GitHub Actions: jobs ordered by `needs`, job-level `if`,
// `strategy.matrix` expansion, `outputs` consumed via the `needs` context,
// step error continuation (later steps skipped unless `if: always()` /
// `failure()`), `continue-on-error`, `working-directory`, GITHUB_ENV /
// GITHUB_PATH / GITHUB_OUTPUT, and the standard expression functions
// (fromJSON, contains, hashFiles, success/failure/cancelled/always, ...).

import { parse as parseYaml } from "@std/yaml";
import { join, resolve } from "@std/path";
import { globToRegExp } from "@std/path/posix";
import {
  type Cache,
  type GitHubCheckSuiteAnnotation,
  type JobResult,
  type PolicyEngineRequest,
  type PolicyEngineStatus,
  type PolicyEngineWorkflow,
  type PolicyEngineWorkflowJob,
  type PolicyEngineWorkflowJobStep,
  StatusComplete,
  type Task,
  WorkflowExecutionContext,
} from "./models.ts";
import { Debug, Info, LogError, Trace, Warn } from "./logger.ts";
import { ExpressionEvaluator } from "./eval.ts";
import { runActionInWorker } from "./action_worker.ts";
import { resolveSandboxConfig, type SandboxConfig } from "./config.ts";
import { type FsApiServer, startFsApiServer } from "./fs_api.ts";

export const EXPRESSION_DENIED = "the policy expression was denied";

const PERMISSION_DENIED = /Requires (read|write|env|net|run|ffi|sys|import)|NotCapable|PermissionDenied/;

/** Parse a workflow from a string (YAML) or an object. */
export function parseWorkflow(input: unknown): PolicyEngineWorkflow {
  if (typeof input === "string") {
    return parseYaml(input) as PolicyEngineWorkflow;
  }
  if (input && typeof input === "object") {
    return input as PolicyEngineWorkflow;
  }
  throw new Error(`unsupported workflow type: ${typeof input}`);
}

interface ActionStep {
  run?: string;
  shell?: string;
  env?: Record<string, string>;
  uses?: string;
  with?: Record<string, unknown>;
  id?: string;
  if?: unknown;
}

interface ActionDef {
  runs?: {
    using?: string;
    main?: string;
    steps?: ActionStep[];
  };
  inputs?: Record<string, { default?: string }>;
  outputs?: Record<string, { value?: string }>;
}

/** Expand a strategy.matrix object into a list of matrix contexts. */
export function expandMatrix(matrix: unknown): Record<string, unknown>[] {
  if (!matrix || typeof matrix !== "object" || Array.isArray(matrix)) return [{}];
  const obj = matrix as Record<string, unknown>;
  const keys = Object.keys(obj);
  if (keys.length === 0) return [{}];
  const values: unknown[][] = keys.map((k) => Array.isArray(obj[k]) ? obj[k] : [obj[k]]);
  let combos: Record<string, unknown>[] = [{}];
  for (let i = 0; i < keys.length; i++) {
    const next: Record<string, unknown>[] = [];
    for (const c of combos) {
      for (const v of values[i]) next.push({ ...c, [keys[i]]: v });
    }
    combos = next;
  }
  return combos;
}

/** Topologically order job names so `needs` dependencies run first. */
export function topoSort(names: string[], needs: Record<string, string[]>): string[] {
  const out: string[] = [];
  const visited = new Set<string>();
  const visiting = new Set<string>();
  const visit = (n: string): void => {
    if (visited.has(n)) return;
    if (visiting.has(n)) throw new Error(`circular needs involving job ${n}`);
    visiting.add(n);
    for (const dep of needs[n] ?? []) visit(dep);
    visiting.delete(n);
    visited.add(n);
    out.push(n);
  };
  for (const n of names) visit(n);
  return out;
}

/** Shells not supported by the engine. Anything else is exec'd as-is. */
function resolveShellParts(shell: string): string[] {
  const parts = shell.trim().split(/\s+/).filter((p) => p.length > 0);
  if (parts.length === 0) return ["bash", "-xe"];
  if (["pwsh", "powershell", "cmd"].includes(parts[0])) {
    throw new Error(
      `shell "${parts[0]}" is not supported by the policy engine (bash/sh/python only)`,
    );
  }
  return parts;
}

async function sha256Bytes(...chunks: Uint8Array[]): Promise<string> {
  const total = chunks.reduce((a, b) => a + b.length, 0);
  const buf = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    buf.set(c, off);
    off += c.length;
  }
  const digest = await crypto.subtle.digest("SHA-256", buf);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Executes workflows. */
export class WorkflowExecutor {
  ctx: WorkflowExecutionContext;
  task: Task | null = null;
  readonly sandbox: SandboxConfig;
  private evaluator: ExpressionEvaluator;
  private denoPath: string;
  // Lazily started FS API server (when sandbox.fsApi is true).
  private fsApiServer: FsApiServer | null = null;
  // Whether the current job has failed (drives success()/failure() and the
  // after-failure step-skip rule).
  private jobFailed = false;
  private jobDeadline = Infinity;
  // Concurrency: workflow-level group plus a per-group serialization gate.
  private workflowConcurrency: string | null = null;
  private concurrencyQueues = new Map<string, Promise<void>>();

  constructor(opts: { sandbox?: SandboxConfig; denoPath?: string } = {}) {
    this.ctx = new WorkflowExecutionContext();
    this.sandbox = opts.sandbox ?? resolveSandboxConfig();
    this.denoPath = opts.denoPath ?? Deno.execPath();
    // The expression sandbox is granted network access only in net-only mode;
    // it never receives filesystem or subprocess permissions.
    this.evaluator = new ExpressionEvaluator({ allowNet: this.sandbox.netOnly });
  }

  /** Execute a parsed workflow and return its final status. */
  async executeWorkflow(request: PolicyEngineRequest): Promise<PolicyEngineStatus> {
    Info("executing workflow");
    const workflow = parseWorkflow(request.workflow);
    const jobs = workflow.jobs ?? {};
    Info("workflow parsed: name=%q jobs=%d", workflow.name ?? "", Object.keys(jobs).length);

    await this.initializeContext(request);
    Debug("context initialized: workspace=%s", this.ctx.workspace);
    if (workflow.name) this.ctx.env["GITHUB_WORKFLOW"] = String(workflow.name);
    if (workflow.env) {
      for (const [k, v] of Object.entries(workflow.env)) {
        this.ctx.env[k] = await this.evaluateExpression(String(v));
      }
    }
    if (workflow.defaults?.run?.shell) this.ctx.shell = workflow.defaults.run.shell;

    if (this.sandbox.fsApi) {
      const fsRoot = this.ctx.workspace || await Deno.makeTempDir({ prefix: "pe-fsapi-" });
      this.fsApiServer = await startFsApiServer(fsRoot);
      Info("FS API server started at %s (root=%s)", this.fsApiServer.url, fsRoot);
    }

    // Expand matrix jobs and topologically order by `needs`.
    const needsMap: Record<string, string[]> = {};
    for (const [jobName, job] of Object.entries(jobs)) {
      needsMap[jobName] = typeof job.needs === "string"
        ? [job.needs]
        : (Array.isArray(job.needs) ? job.needs : []);
    }
    let order: string[];
    try {
      order = topoSort(Object.keys(jobs), needsMap);
    } catch (err) {
      return this.createErrorStatus(err as Error);
    }
    this.workflowConcurrency = await this.resolveConcurrency(workflow.concurrency);

    let anyFailed = false;
    let firstError: Error | null = null;

    try {
      for (const jobName of order) {
        const job = jobs[jobName];
        const combos = expandMatrix(job.strategy?.matrix);
        for (let i = 0; i < combos.length; i++) {
          const matrix = combos[i];
          const key = combos.length > 1 ? `${jobName}-${i + 1}` : jobName;
          const result = await this.runJobInstance(jobName, key, job, matrix);
          // needs context is keyed by base job name (last matrix instance wins).
          this.ctx.jobResults[jobName] = result;
          if (combos.length > 1) this.ctx.jobResults[key] = result;
          if (result.status === "failure") {
            anyFailed = true;
            firstError = firstError ?? result.error ?? new Error(`job ${key} failed`);
          }
        }
      }
    } finally {
      if (this.fsApiServer) {
        await this.fsApiServer.close();
        this.fsApiServer = null;
      }
      // Clean up ephemeral directories. cacheDir is intentionally kept.
      await this.removeAll(this.ctx.workspace);
      await this.removeAll(this.ctx.toolCacheDir);
      await this.removeAll(this.ctx.homeDir);
      this.evaluator.close();
    }

    if (anyFailed) {
      return this.createErrorStatus(firstError ?? new Error("workflow failed"));
    }
    Info("workflow completed successfully");
    return this.createSuccessStatus();
  }

  private async removeAll(path: string): Promise<void> {
    if (!path) return;
    try {
      await Deno.remove(path, { recursive: true });
    } catch {
      // ignore
    }
  }

  /** Initialize the execution context from the request. */
  private async initializeContext(request: PolicyEngineRequest): Promise<void> {
    if (request.inputs) {
      Object.assign(this.ctx.inputs, request.inputs);
    }

    const context = request.context;
    if (context) {
      const config = context["config"] as Record<string, unknown> | undefined;
      const env = config?.["env"] as Record<string, unknown> | undefined;
      if (env) Object.assign(this.ctx.env, env);

      const secrets = context["secrets"] as Record<string, unknown> | undefined;
      if (secrets) {
        for (const [k, v] of Object.entries(secrets)) {
          if (typeof v === "string") this.ctx.secrets[k] = v;
        }
      }

      // Seed the in-memory cache from the request. Actions read it through the
      // GITHUB_CACHE command file and may write an updated map back.
      const cache = context["cache"] as Cache | undefined;
      if (cache && typeof cache === "object") this.ctx.cache = cache;

      // Repository variables, exposed to expressions as the vars context.
      const vars = context["vars"] as Record<string, unknown> | undefined;
      if (vars && typeof vars === "object") this.ctx.vars = vars;
    }

    // GITHUB_TOKEN is available as both a secret and an env var in real Actions.
    const token = this.ctx.secrets["GITHUB_TOKEN"];
    if (token && this.ctx.env["GITHUB_TOKEN"] === undefined) {
      this.ctx.env["GITHUB_TOKEN"] = token;
    }

    // Default runner env present in both sandbox modes.
    this.ctx.env["CI"] = "true";
    this.ctx.env["GITHUB_ACTIONS"] = "true";
    // Seed PATH from the host so GITHUB_PATH prepends extend a real path.
    if (this.ctx.env["PATH"] === undefined) {
      this.ctx.env["PATH"] = Deno.env.get("PATH") ?? "";
    }

    // In net-only mode the engine must never touch the filesystem, so the
    // ephemeral workspace/cache/home directories are not created. Any step
    // that would need them is refused later (see assertExecAllowed).
    if (this.sandbox.netOnly) {
      Info("net-only sandbox: filesystem and subprocess execution are disabled");
      return;
    }

    const cwd = Deno.cwd();
    this.ctx.cacheDir = join(cwd, ".cache");
    this.ctx.tempDir = join(cwd, ".tempdir");
    await Deno.mkdir(this.ctx.cacheDir, { recursive: true });
    await Deno.mkdir(this.ctx.tempDir, { recursive: true });

    this.ctx.workspace = await Deno.makeTempDir({ dir: this.ctx.tempDir, prefix: "workspace-" });
    this.ctx.toolCacheDir = await Deno.makeTempDir({ dir: this.ctx.tempDir, prefix: "toolcache-" });
    this.ctx.homeDir = await Deno.makeTempDir({ dir: this.ctx.tempDir, prefix: "home-" });

    // GITHUB_EVENT_PATH: the JSON event payload file actions expect. Lite only
    // knows about workflow inputs, so that is what the file carries.
    const eventPath = join(this.ctx.tempDir, "github_event.json");
    await Deno.writeTextFile(eventPath, JSON.stringify({ inputs: this.ctx.inputs }));
    this.ctx.env["GITHUB_EVENT_PATH"] = eventPath;
  }

  /**
   * Refuse operations that require the filesystem or subprocess execution when
   * running in the net-only sandbox.
   */
  private assertExecAllowed(what: string): void {
    if (this.sandbox.netOnly) {
      throw new Error(
        `${what} requires filesystem/exec access, which is disabled in net-only sandbox mode`,
      );
    }
  }

  /** Run one job (or matrix instance). Returns its result for the needs context. */
  private async runJobInstance(
    jobName: string,
    key: string,
    job: PolicyEngineWorkflowJob,
    matrix: Record<string, unknown>,
  ): Promise<JobResult> {
    Info("starting job: %s (steps=%d)", key, job.steps?.length ?? 0);
    this.ctx.matrix = matrix;

    const deps: string[] = typeof job.needs === "string"
      ? [job.needs]
      : (Array.isArray(job.needs) ? job.needs : []);
    const depFailed = deps.some((n) => {
      const r = this.ctx.jobResults[n];
      return r !== undefined && (r.status === "failure" || r.status === "skipped");
    });

    // Job-level if. failure()/success() reflect whether a dependency failed.
    if (job.if !== undefined && job.if !== null) {
      this.jobFailed = depFailed;
      const guard = await this.evaluateGuard(job.if);
      if (!guard.run) {
        Info("job %s skipped (if condition false)", key);
        return { status: "skipped", outputs: {} };
      }
    } else if (depFailed) {
      Info("job %s skipped (dependency failed)", key);
      return { status: "skipped", outputs: {} };
    }

    // Concurrency: jobs sharing a group run one at a time. In this serial
    // engine the gate is never contended, but it keeps group semantics explicit
    // (and correct if jobs are ever parallelized).
    const group = await this.resolveConcurrency(job.concurrency ?? this.workflowConcurrency);
    if (group) {
      const prev = this.concurrencyQueues.get(group) ?? Promise.resolve();
      let release!: () => void;
      const next = new Promise<void>((r) => (release = r));
      this.concurrencyQueues.set(group, prev.then(() => next));
      await prev;
      try {
        return await this.runJobBody(jobName, key, job, matrix);
      } finally {
        release();
      }
    }
    return await this.runJobBody(jobName, key, job, matrix);
  }

  /** Execute a job's steps, evaluate its outputs, and restore job-scoped env. */
  private async runJobBody(
    jobName: string,
    key: string,
    job: PolicyEngineWorkflowJob,
    matrix: Record<string, unknown>,
  ): Promise<JobResult> {
    this.ctx.matrix = matrix;
    // Job env is scoped to this job: snapshot, apply, restore afterwards.
    const envSnapshot: Record<string, string> = {};
    for (const [k, v] of Object.entries(this.ctx.env)) envSnapshot[k] = String(v);
    if (job.env) {
      for (const [k, v] of Object.entries(job.env)) {
        this.ctx.env[k] = await this.evaluateExpression(String(v));
      }
    }
    if (job.defaults?.run?.shell) this.ctx.shell = job.defaults.run.shell;
    this.jobDeadline = job["timeout-minutes"] != null
      ? Date.now() + Number(job["timeout-minutes"]) * 60_000
      : Infinity;

    let error: Error | null = null;
    try {
      await this.executeJobSteps(job, key);
    } catch (err) {
      error = err as Error;
    }

    // Job outputs: expressions evaluated over the step-outputs/needs/matrix
    // contexts, then consumed by dependent jobs via needs.<job>.outputs.
    const outputs: Record<string, unknown> = {};
    if (job.outputs) {
      for (const [name, expr] of Object.entries(job.outputs)) {
        try {
          outputs[name] = await this.evaluateExpression(String(expr));
        } catch (err) {
          Warn("job %s output %s failed: %v", key, name, err);
          outputs[name] = "";
        }
      }
    }

    // Restore env — GITHUB_ENV updates and job env do not leak across jobs.
    for (const k of Object.keys(this.ctx.env)) {
      if (!(k in envSnapshot)) delete this.ctx.env[k];
    }
    Object.assign(this.ctx.env, envSnapshot);
    this.ctx.matrix = {};

    if (error) {
      LogError("job %s failed: %v", key, error.message);
      return { status: "failure", outputs, error };
    }
    Info("job %s completed successfully", key);
    return { status: "success", outputs };
  }

  /** Resolve a concurrency value (string or {group, cancel-in-progress}) to a group name. */
  private async resolveConcurrency(value: unknown): Promise<string | null> {
    if (value == null) return null;
    if (typeof value === "string") return await this.evaluateExpression(value);
    if (typeof value === "object") {
      const group = (value as { group?: unknown }).group;
      if (group != null) return await this.evaluateExpression(String(group));
    }
    return null;
  }

  /** Execute a job's steps with GitHub-style error continuation. */
  private async executeJobSteps(job: PolicyEngineWorkflowJob, jobKey: string): Promise<void> {
    const steps = job.steps ?? [];
    this.jobFailed = false;
    this.ctx.error = null;

    for (let i = 0; i < steps.length; i++) {
      if (Date.now() > this.jobDeadline) {
        this.jobFailed = true;
        this.ctx.error = new Error(`job ${jobKey} exceeded timeout-minutes`);
        break;
      }

      const step = steps[i];
      let stepName = `step_${i + 1}`;
      if (step.id) stepName = step.id;
      else if (step.name) stepName = step.name;

      let guard = { run: true, always: false, failure: false };
      if (step.if !== undefined && step.if !== null) guard = await this.evaluateGuard(step.if);

      // After a hard failure, only always()/failure() steps still run.
      if (this.jobFailed && !guard.always && !guard.failure) {
        Info("step %s skipped (job failed)", stepName);
        continue;
      }
      if (!guard.run) {
        Info("step %s skipped (if condition false)", stepName);
        continue;
      }

      if (step.shell) this.ctx.shell = step.shell;
      const stepEnv = await this.buildStepEnv(step);
      const wd = step["working-directory"] ?? job.defaults?.run?.["working-directory"];
      const cwd = wd ? join(this.ctx.workspace, wd) : this.ctx.workspace;

      this.emit(`##[group]${stepName}`);

      let err: Error | null = null;
      try {
        if (step.uses) {
          await this.executeStepUses(step, stepEnv, cwd);
        } else if (step.run) {
          await this.executeStepRun(step, stepEnv, cwd);
        }
      } catch (e) {
        err = e as Error;
      }

      this.emit("##[endgroup]");

      if (err) {
        LogError("step %s failed: %v", stepName, err.message);
        this.emit(`##[error]step ${stepName} failed: ${err.message}`);
        if (step["continue-on-error"]) {
          Warn("step %s failed but continue-on-error=true; continuing", stepName);
          continue;
        }
        this.jobFailed = true;
        this.ctx.error = err;
        // Keep looping so always()/failure() steps can still run.
      } else {
        Info("step %s completed", stepName);
      }
    }

    // A single fast step never trips the in-loop deadline check; verify once more.
    if (Date.now() > this.jobDeadline) {
      this.jobFailed = true;
      this.ctx.error = new Error(`job ${jobKey} exceeded timeout-minutes`);
    }

    if (this.jobFailed) throw this.ctx.error ?? new Error(`job ${jobKey} failed`);
  }

  /** Append a marker line to both the snapshot buffer and the live task stream. */
  private emit(line: string): void {
    this.ctx.consoleOutput.push(line);
    if (this.task) this.task.appendConsoleOutput(line);
  }

  /**
   * Evaluate a step/job `if` condition and classify it. Mirrors GitHub: after a
   * failure only `always()`/`failure()` steps run; `failure()` is false while
   * the job has not failed.
   */
  async evaluateGuard(
    condition: unknown,
  ): Promise<{ run: boolean; always: boolean; failure: boolean }> {
    const condStr = typeof condition === "string" ? condition : "";
    const hasAlways = /(?:^|[^A-Za-z0-9_])always\(\)/.test(condStr);
    const hasFailure = /(?:^|[^A-Za-z0-9_])failure\(\)/.test(condStr);

    let run = true;
    if (typeof condition === "boolean") {
      run = condition;
    } else if (typeof condition === "number") {
      run = condition !== 0;
    } else if (typeof condition === "string") {
      if (condition === "") {
        run = false;
      } else {
        const trimmed = condition.trim().toLowerCase();
        if (trimmed === "true" || trimmed === "1") {
          run = true;
        } else if (trimmed === "false" || trimmed === "0") {
          run = false;
        } else {
          let expr = condition;
          if (!expr.includes("${{")) expr = "${{ " + condition + " }}";
          let evaluated = await this.evaluateExpression(expr);
          if (evaluated.includes("${{")) {
            throw new Error(`could not evaluate expression ${JSON.stringify(condition)}`);
          }
          evaluated = evaluated.trim().toLowerCase();
          if (evaluated === "__github_actions_always__") {
            run = true;
          } else if (
            evaluated === "false" || evaluated === "0" || evaluated === "" ||
            evaluated === "null" || evaluated === "undefined"
          ) {
            run = false;
          } else {
            run = true;
          }
        }
      }
    }

    const failed = this.jobFailed;
    if (failed) {
      if (hasAlways || hasFailure) run = true;
      else run = false;
    } else if (hasFailure) {
      run = false;
    }
    return { run, always: hasAlways, failure: hasFailure };
  }

  /** Legacy boolean-only condition evaluation (used by callers/tests). */
  async evaluateCondition(condition: unknown): Promise<boolean> {
    return (await this.evaluateGuard(condition)).run;
  }

  /** Build the environment map for a step. */
  async buildStepEnv(step: PolicyEngineWorkflowJobStep): Promise<Record<string, string>> {
    const env: Record<string, string> = {};

    for (const [k, v] of Object.entries(this.ctx.env)) {
      env[k] = String(v);
    }
    for (const [k, v] of Object.entries(step.env ?? {})) {
      env[k] = await this.evaluateExpression(String(v));
    }
    for (const [k, v] of Object.entries(step.with ?? {})) {
      env["INPUT_" + k.toUpperCase()] = await this.evaluateExpression(String(v));
    }

    env["GITHUB_WORKSPACE"] = this.ctx.workspace;
    env["RUNNER_TEMP"] = this.ctx.tempDir;
    env["RUNNER_TOOL_CACHE"] = this.ctx.toolCacheDir;
    env["AGENT_TOOLSDIRECTORY"] = this.ctx.toolCacheDir;
    env["HOME"] = this.ctx.homeDir;

    if (this.fsApiServer) {
      env["POLICY_ENGINE_FS_API_URL"] = this.fsApiServer.url;
    }

    return env;
  }

  /** Evaluate all ${{ ... }} occurrences in a string. */
  async evaluateExpression(expr: string): Promise<string> {
    if (typeof expr !== "string") return String(expr);
    if (!expr.includes("${{")) return expr;
    const re = /\$\{\{\s*([\s\S]+?)\s*\}\}/g;
    let result = "";
    let last = 0;
    let m: RegExpExecArray | null;
    while ((m = re.exec(expr)) !== null) {
      result += expr.slice(last, m.index);
      result += await this.evaluateInnerExpression(m[1].trim());
      last = m.index + m[0].length;
    }
    result += expr.slice(last);
    return result;
  }

  /**
   * Evaluate a single expression (content between ${{ and }}) natively in the
   * sandboxed worker. Falls back to simple property-path resolution on error.
   */
  private async evaluateInnerExpression(inner: string): Promise<string> {
    try {
      return await this.evaluateUsingJavaScript(inner);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      if (PERMISSION_DENIED.test(message)) {
        throw new Error(`${EXPRESSION_DENIED}: ${message}`);
      }
      // Fall back to simple property-path resolution.
      const data: Record<string, unknown> = {
        github: this.buildGitHubContext(),
        steps: this.ctx.outputs,
        inputs: this.ctx.inputs,
        env: this.ctx.env,
        secrets: this.ctx.secrets,
        vars: this.ctx.vars,
        needs: this.ctx.jobResults,
        matrix: this.ctx.matrix,
      };
      const value = resolvePropertyPath(inner, data);
      if (value !== undefined && value !== null) return String(value);
      return "${{ " + inner + " }}";
    }
  }

  /**
   * Evaluate an expression as JavaScript inside the permission-restricted
   * worker. The github, runner, steps, inputs, env, secrets, vars, needs, and
   * matrix contexts are embedded in a self-contained IIFE — no subprocess is
   * spawned and no file is written. Standard GitHub expression functions
   * (fromJSON, contains, hashFiles, success/failure/cancelled/always, ...) are
   * provided in scope; hashFiles is resolved host-side against the workspace.
   */
  private async evaluateUsingJavaScript(codeBlock: string): Promise<string> {
    const githubCtx = this.buildGitHubContext();
    const stepsCtx = this.ctx.outputs;
    const inputsCtx = this.ctx.inputs;
    const runnerCtx = { debug: 1 };

    const resolved = await this.resolveHashFiles(codeBlock);
    const transformed = transformPropertyAccessors(resolved);

    const jsCode = `(() => {
function always() { return "__GITHUB_ACTIONS_ALWAYS__"; }
function success() { return !__pe_failed; }
function failure() { return __pe_failed; }
function cancelled() { return false; }
function fromJSON(s) { return JSON.parse(s); }
function toJSON(v) { return JSON.stringify(v); }
function contains(h, n) {
  if (Array.isArray(h)) return h.includes(n);
  if (typeof h === "string") return h.includes(n);
  return false;
}
function startsWith(h, n) { return typeof h === "string" && h.startsWith(n); }
function endsWith(h, n) { return typeof h === "string" && h.endsWith(n); }
function format(s) {
  const args = Array.prototype.slice.call(arguments, 1);
  return String(s).replace(/\\{(\\d+)\\}/g, (m, i) => args[Number(i)] !== undefined ? String(args[Number(i)]) : m);
}
function join(a, sep) { return Array.isArray(a) ? a.join(sep === undefined ? "," : sep) : String(a); }
const __pe_failed = ${this.jobFailed ? "true" : "false"};
const github = ${JSON.stringify(githubCtx)};
const runner = ${JSON.stringify(runnerCtx)};
const steps = ${JSON.stringify(stepsCtx)};
const inputs = ${JSON.stringify(inputsCtx)};
const env = ${JSON.stringify(this.ctx.env)};
const secrets = ${JSON.stringify(this.ctx.secrets)};
const vars = ${JSON.stringify(this.ctx.vars)};
const needs = ${JSON.stringify(this.ctx.jobResults)};
const matrix = ${JSON.stringify(this.ctx.matrix)};
return (${transformed});
})()`;
    Trace("evaluateUsingJavaScript(%s): %s", codeBlock, jsCode);

    return await this.evaluator.evaluate(jsCode);
  }

  /** Build the github context for expression evaluation. Mirrors the original
   * Python engine: every GITHUB_* env var becomes github.<lowercased-suffix>,
   * e.g. GITHUB_SHA → github.sha. token falls back to the GITHUB_TOKEN secret. */
  buildGitHubContext(): Record<string, unknown> {
    const ctx: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(this.ctx.env)) {
      if (key.startsWith("GITHUB_")) {
        ctx[key.toLowerCase().replace("github_", "")] = value;
      }
    }
    if (ctx["token"] === undefined) {
      ctx["token"] = this.ctx.secrets["GITHUB_TOKEN"] ?? "";
    }
    ctx["event"] = { inputs: this.ctx.inputs };
    return ctx;
  }

  /**
   * Resolve hashFiles('pattern') calls host-side (the expression worker has no
   * filesystem access). Each call is replaced by its sha256 as a JSON string.
   */
  private async resolveHashFiles(code: string): Promise<string> {
    const re = /hashFiles\(\s*(['"])((?:(?!\1).)*)\1\s*\)/g;
    let result = "";
    let last = 0;
    let m: RegExpExecArray | null;
    while ((m = re.exec(code)) !== null) {
      result += code.slice(last, m.index);
      const hash = await this.computeHashFiles([m[2]]);
      result += JSON.stringify(hash);
      last = m.index + m[0].length;
    }
    result += code.slice(last);
    return result;
  }

  /** GHA-style hash of all files under the workspace matching the patterns. */
  private async computeHashFiles(patterns: string[]): Promise<string> {
    if (this.sandbox.netOnly || !this.ctx.workspace) return "";
    const files: string[] = [];
    for (const p of patterns) {
      let re: RegExp;
      try {
        re = globToRegExp(p, { extended: true, globstar: true });
      } catch {
        continue;
      }
      for (const f of await this.walkWorkspace()) {
        const rel = f.startsWith(this.ctx.workspace + "/")
          ? f.slice(this.ctx.workspace.length + 1)
          : f;
        if (re.test(rel)) files.push(f);
      }
    }
    files.sort();
    const chunks: Uint8Array[] = [];
    for (const f of files) {
      try {
        chunks.push(await Deno.readFile(f));
      } catch {
        // skip missing
      }
    }
    return await sha256Bytes(...chunks);
  }

  private async walkWorkspace(): Promise<string[]> {
    const out: string[] = [];
    const walk = async (dir: string): Promise<void> => {
      let entries: Deno.DirEntry[] = [];
      try {
        entries = [...Deno.readDirSync(dir)];
      } catch {
        return;
      }
      for (const e of entries) {
        const full = join(dir, e.name);
        if (e.isDirectory) await walk(full);
        else out.push(full);
      }
    };
    await walk(this.ctx.workspace);
    return out;
  }

  /** Execute a step that uses an action. */
  private async executeStepUses(
    step: PolicyEngineWorkflowJobStep,
    env: Record<string, string>,
    cwd: string,
  ): Promise<void> {
    const uses = step.uses!;
    let actionPath = "";

    if (uses.startsWith("./") || uses.startsWith("/")) {
      actionPath = uses.startsWith("/") ? uses : join(this.ctx.workspace, uses);
      if (!(await exists(actionPath))) {
        throw new Error(`local action path does not exist: ${actionPath}`);
      }
      Debug("action resolved from local path: %s", actionPath);
    } else if (uses.includes("@")) {
      const [orgRepo, version] = splitN(uses, "@", 2);

      let repoActionsDir = env["ACTIONS_DIR"];
      if (!repoActionsDir) repoActionsDir = join(this.ctx.workspace, ".tangled", "actions");
      const repoLocal = join(repoActionsDir, orgRepo);
      if (await exists(repoLocal)) {
        actionPath = repoLocal;
        Debug("action %s resolved from repo-supplied dir: %s", orgRepo, actionPath);
      }

      if (!actionPath) {
        const bundledDir = this.sandbox.bundledActionsDir ?? Deno.env.get("BUNDLED_ACTIONS_DIR");
        if (bundledDir) {
          const bundledPath = join(bundledDir, orgRepo);
          if (await exists(bundledPath)) {
            actionPath = bundledPath;
            Debug("action %s resolved from bundled dir: %s", orgRepo, actionPath);
          }
        }
      }

      if (!actionPath) {
        // Downloading requires writing to the cache directory; not available
        // in the net-only sandbox. Actions must be supplied via a local path,
        // ACTIONS_DIR, or BUNDLED_ACTIONS_DIR.
        if (this.sandbox.netOnly) {
          throw new Error(
            `cannot download action ${orgRepo}@${version} in net-only mode; ` +
              `provide it via a local path, ACTIONS_DIR, or BUNDLED_ACTIONS_DIR`,
          );
        }
        Debug("downloading action %s@%s from GitHub", orgRepo, version);
        actionPath = await this.downloadAction(orgRepo, version);
        Debug("action %s downloaded to: %s", orgRepo, actionPath);
      }
    } else {
      throw new Error(
        `unsupported uses format (expected org/repo@version or ./path): ${uses}`,
      );
    }

    // Action subprocesses run with cwd = workspace; make the action path
    // absolute so a relative BUNDLED_ACTIONS_DIR / ACTIONS_DIR still resolves.
    actionPath = resolve(actionPath);

    let actionYamlPath = join(actionPath, "action.yml");
    if (!(await exists(actionYamlPath))) actionYamlPath = join(actionPath, "action.yaml");
    const actionDef = parseYaml(await Deno.readTextFile(actionYamlPath)) as ActionDef;

    // Add default inputs.
    for (const [inputName, inputDef] of Object.entries(actionDef.inputs ?? {})) {
      const envKey = "INPUT_" + inputName.toUpperCase();
      if (env[envKey] === undefined && inputDef.default) {
        env[envKey] = await this.evaluateExpression(inputDef.default);
      }
    }

    env["GITHUB_ACTION_PATH"] = actionPath;
    env["GITHUB_ACTION"] = step.id || step.uses || "";

    const using = actionDef.runs?.using ?? "";
    Debug("executing action type: %s", using);
    if (using.startsWith("node")) {
      // In net-only mode the action runs in the permission-restricted worker;
      // otherwise it runs as a full Deno subprocess (which also supports
      // CommonJS/ncc bundles).
      if (this.sandbox.netOnly) {
        await this.executeNodeActionSandboxed(actionPath, actionDef.runs?.main ?? "", env, step.id);
      } else {
        await this.executeNodeActionSubprocess(
          actionPath,
          actionDef.runs?.main ?? "",
          env,
          step.id,
          cwd,
        );
      }
    } else if (using === "composite") {
      // Composite actions execute shell steps, which require process execution.
      this.assertExecAllowed("composite action");
      await this.executeCompositeAction(actionDef, actionDef.runs?.steps ?? [], env, cwd);
    } else {
      throw new Error(`unsupported action type: ${using}`);
    }
  }

  /**
   * Run a JS/TS action's source in the sandboxed worker (net-only). The action
   * gets network access (if enabled) but no filesystem or subprocess access;
   * GITHUB_OUTPUT/GITHUB_ENV writes are captured via an in-memory virtual FS.
   */
  private async executeNodeActionSandboxed(
    actionPath: string,
    main: string,
    env: Record<string, string>,
    stepID: string | undefined,
  ): Promise<void> {
    const source = await Deno.readTextFile(join(actionPath, main));
    const result = await runActionInWorker({
      source,
      // ponytail: a TypeScript action main still leaves one DENO_DIR gen/blob emit per run; ship it as bundled JavaScript to avoid that
      typescript: /\.[cm]?tsx?$/.test(main),
      env,
      allowNet: this.sandbox.netOnly, // net granted in net-only mode
      cache: JSON.stringify(this.ctx.cache),
      event: JSON.stringify({ inputs: this.ctx.inputs }),
      onLine: (line) => {
        Trace("| %s", line);
        this.ctx.consoleOutput.push(line);
        if (this.task) this.task.appendConsoleOutput(line);
        this.parseAnnotations(line);
      },
    });

    if (stepID && result.output) {
      const outputs = parseGitHubActionsOutputs(result.output);
      if (!this.ctx.outputs[stepID]) this.ctx.outputs[stepID] = {};
      this.ctx.outputs[stepID]["outputs"] = outputs;
    }
    if (result.env) {
      for (const [k, v] of Object.entries(parseGitHubActionsOutputs(result.env))) {
        this.ctx.env[k] = v;
      }
    }

    // Replace the in-memory cache with whatever the action wrote back. Each
    // action sees the current cache and returns the full updated map; a failed
    // parse keeps the previous cache.
    this.ctx.cache = parseCache(result.cache, this.ctx.cache);

    if (result.code !== 0) {
      throw new Error(`action exited with code ${result.code}`);
    }
  }

  /** Download a GitHub Action to the cache directory and return its path.
   * Supports subdir actions (org/repo/path@version): the repo is downloaded and
   * the subdirectory returned. */
  private async downloadAction(orgRepo: string, version: string): Promise<string> {
    const parts = orgRepo.split("/");
    const org = parts[0] ?? "";
    const repo = parts[1] ?? "";
    const sub = parts.slice(2).join("/");
    if (!org || !repo) throw new Error(`invalid uses org/repo: ${orgRepo}`);

    const extractedRoot = join(this.ctx.cacheDir, org, repo, "extracted");
    if (await exists(extractedRoot)) {
      return sub ? join(extractedRoot, sub) : extractedRoot;
    }

    const downloadDir = join(this.ctx.cacheDir, org, repo);
    await Deno.mkdir(downloadDir, { recursive: true });

    const urls = [
      `https://github.com/${org}/${repo}/archive/refs/tags/${version}.zip`,
      `https://github.com/${org}/${repo}/archive/${version}.zip`,
      `https://github.com/${org}/${repo}/archive/refs/heads/${version}.zip`,
    ];

    let downloadErr: Error | null = null;
    for (const downloadURL of urls) {
      const compressedPath = join(downloadDir, "compressed.zip");
      try {
        const headers: HeadersInit = {};
        const token = this.ctx.secrets["GITHUB_TOKEN"];
        if (token) headers["Authorization"] = "Bearer " + token;

        const resp = await fetch(downloadURL, { headers });
        if (!resp.ok) {
          downloadErr = new Error(`download failed with status: ${resp.status}`);
          continue;
        }
        await Deno.writeFile(compressedPath, new Uint8Array(await resp.arrayBuffer()));

        const extractedTmpPath = join(downloadDir, "extracted_tmp");
        await unzip(compressedPath, extractedTmpPath);

        const entries: Deno.DirEntry[] = [];
        for await (const e of Deno.readDir(extractedTmpPath)) entries.push(e);
        if (entries.length === 0) {
          downloadErr = new Error("failed to find extracted directory");
          continue;
        }

        const srcDir = join(extractedTmpPath, entries[0].name);
        await Deno.rename(srcDir, extractedRoot);
        await this.removeAll(extractedTmpPath);
        try {
          await Deno.remove(compressedPath);
        } catch { /* ignore */ }

        if (sub) {
          const p = join(extractedRoot, sub);
          if (!(await exists(p))) {
            throw new Error(`subdirectory ${sub} not found in ${org}/${repo}`);
          }
          return p;
        }
        return extractedRoot;
      } catch (e) {
        downloadErr = e as Error;
      }
    }
    throw new Error(`failed to download action from any URL: ${downloadErr?.message}`);
  }

  /**
   * Execute a Node.js action as a full Deno subprocess (Node compatibility
   * layer). Used in the default sandbox; supports CommonJS/ncc bundles.
   */
  private async executeNodeActionSubprocess(
    actionPath: string,
    main: string,
    env: Record<string, string>,
    stepID: string | undefined,
    cwd: string,
  ): Promise<void> {
    const ghFiles: Array<[string, string]> = [
      ["GITHUB_OUTPUT", "node-output-"],
      ["GITHUB_ENV", "node-env-"],
      ["GITHUB_PATH", "node-path-"],
      ["GITHUB_STATE", "node-state-"],
      ["GITHUB_CACHE", "node-cache-"],
    ];
    const cleanup: string[] = [];
    for (const [envKey, prefix] of ghFiles) {
      const f = await Deno.makeTempFile({ dir: this.ctx.tempDir, prefix, suffix: ".txt" });
      env[envKey] = f;
      cleanup.push(f);
    }

    // Seed the cache command file with the current in-memory cache.
    await Deno.writeTextFile(env["GITHUB_CACHE"], JSON.stringify(this.ctx.cache));

    // Point the subprocess deno at the persistent download cache so jsr: deps
    // (e.g. @std/path pulled by bundled actions) are fetched once, not per run
    // into the ephemeral HOME.
    if (env["DENO_DIR"] === undefined) env["DENO_DIR"] = this.ctx.cacheDir;

    try {
      // Many actions are ncc-bundled CommonJS. Deno defaults a type-less
      // package.json to ESM, leaving __dirname undefined — force commonjs.
      const actionPkgPath = join(actionPath, "package.json");
      try {
        const pkg = JSON.parse(await Deno.readTextFile(actionPkgPath));
        if (pkg.type === undefined) {
          pkg.type = "commonjs";
          await Deno.writeTextFile(actionPkgPath, JSON.stringify(pkg));
        }
      } catch {
        await Deno.writeTextFile(actionPkgPath, `{"type":"commonjs"}`);
      }

      const mainPath = join(actionPath, main);
      const cmd = new Deno.Command(this.denoPath, {
        args: denoRunArgs("--allow-all", "--no-prompt", mainPath),
        cwd,
        env: env,
        stdout: "piped",
        stderr: "piped",
      });
      const output = await this.runAndStreamOutput(cmd);
      this.ctx.consoleOutput.push(output);

      // Parse outputs from GITHUB_OUTPUT.
      if (stepID) {
        const content = await readTextOrEmpty(env["GITHUB_OUTPUT"]);
        if (content) {
          const outputs = parseGitHubActionsOutputs(content);
          if (!this.ctx.outputs[stepID]) this.ctx.outputs[stepID] = {};
          this.ctx.outputs[stepID]["outputs"] = outputs;
        }
      }

      // Apply env updates from GITHUB_ENV.
      const envContent = await readTextOrEmpty(env["GITHUB_ENV"]);
      if (envContent) {
        for (const [k, v] of Object.entries(parseGitHubActionsOutputs(envContent))) {
          this.ctx.env[k] = v;
        }
      }

      // Apply PATH updates from GITHUB_PATH.
      this.applyPathFile(env["GITHUB_PATH"]);

      // Replace the in-memory cache with whatever the action wrote back.
      this.ctx.cache = parseCache(await readTextOrEmpty(env["GITHUB_CACHE"]), this.ctx.cache);
    } finally {
      for (const f of cleanup) {
        try {
          await Deno.remove(f);
        } catch { /* ignore */ }
      }
    }
  }

  /** Execute a composite action's steps (run and nested uses) and outputs. */
  private async executeCompositeAction(
    actionDef: ActionDef,
    steps: ActionStep[],
    env: Record<string, string>,
    cwd: string,
  ): Promise<void> {
    for (let i = 0; i < steps.length; i++) {
      const step = steps[i];

      let guard = { run: true, always: false, failure: false };
      if (step.if !== undefined && step.if !== null) guard = await this.evaluateGuard(step.if);
      if (this.jobFailed && !guard.always && !guard.failure) continue;
      if (!guard.run) continue;

      const stepEnv = { ...env };
      for (const [k, v] of Object.entries(step.env ?? {})) {
        stepEnv[k] = await this.evaluateExpression(v);
      }

      if (step.uses) {
        await this.executeStepUses(
          step as unknown as PolicyEngineWorkflowJobStep,
          stepEnv,
          cwd,
        );
      } else if (step.run) {
        const shell = step.shell || this.ctx.shell;
        await this.runShellCommand(
          await this.evaluateExpression(step.run),
          shell,
          stepEnv,
          cwd,
        );
      }
    }

    // Composite action outputs (value expressions over the steps context).
    const outFile = env["GITHUB_OUTPUT"];
    for (const [name, outDef] of Object.entries(actionDef.outputs ?? {})) {
      const expr = outDef?.value;
      if (expr && outFile) {
        const val = await this.evaluateExpression(expr);
        await Deno.writeTextFile(outFile, `${name}=${val}\n`, { append: true });
      }
    }
  }

  /** Execute a `run` step. */
  private async executeStepRun(
    step: PolicyEngineWorkflowJobStep,
    env: Record<string, string>,
    cwd: string,
  ): Promise<void> {
    this.assertExecAllowed("run step");
    const runScript = await this.evaluateExpression(step.run!);

    const outputFile = await Deno.makeTempFile({
      dir: this.ctx.tempDir,
      prefix: "output-",
      suffix: ".txt",
    });
    const envFile = await Deno.makeTempFile({
      dir: this.ctx.tempDir,
      prefix: "env-",
      suffix: ".txt",
    });
    const pathFile = await Deno.makeTempFile({
      dir: this.ctx.tempDir,
      prefix: "path-",
      suffix: ".txt",
    });

    env["GITHUB_OUTPUT"] = outputFile;
    env["GITHUB_ENV"] = envFile;
    env["GITHUB_PATH"] = pathFile;

    const shell = step.shell || this.ctx.shell;

    try {
      await this.runShellCommand(runScript, shell, env, cwd);

      if (step.id) {
        const outputs = parseGitHubActionsOutputs(await readTextOrEmpty(outputFile));
        if (!this.ctx.outputs[step.id]) this.ctx.outputs[step.id] = {};
        this.ctx.outputs[step.id]["outputs"] = outputs;
      }

      for (
        const [k, v] of Object.entries(parseGitHubActionsOutputs(await readTextOrEmpty(envFile)))
      ) {
        this.ctx.env[k] = v;
      }

      this.applyPathFile(pathFile);
    } finally {
      for (const f of [outputFile, envFile, pathFile]) {
        try {
          await Deno.remove(f);
        } catch { /* ignore */ }
      }
    }
  }

  /** Run a shell command, writing the script to a temp file. */
  private async runShellCommand(
    script: string,
    shell: string,
    env: Record<string, string>,
    cwd: string,
  ): Promise<void> {
    Debug("running shell command: shell=%s", shell);
    Trace("script content:\n%s", script);

    const scriptPath = await Deno.makeTempFile({ dir: this.ctx.tempDir, prefix: "script-" });
    try {
      await Deno.writeTextFile(scriptPath, script);

      const shellParts = resolveShellParts(shell);

      // Replace {0} placeholder with the script path.
      const args: string[] = [];
      let found = false;
      for (const part of shellParts) {
        if (part.includes("{0}")) {
          args.push(part.replace("{0}", scriptPath));
          found = true;
        } else {
          args.push(part);
        }
      }
      if (!found) args.push(scriptPath);

      const cmd = new Deno.Command(args[0], {
        args: args.slice(1),
        cwd: cwd,
        env: env,
        stdout: "piped",
        stderr: "piped",
      });
      const output = await this.runAndStreamOutput(cmd);
      this.ctx.consoleOutput.push(output);
      this.parseAnnotations(output);
    } finally {
      try {
        await Deno.remove(scriptPath);
      } catch { /* ignore */ }
    }
  }

  /** Run a command, capturing merged stdout/stderr and streaming lines to the task. */
  private async runAndStreamOutput(cmd: Deno.Command): Promise<string> {
    const child = cmd.spawn();
    const decoder = new TextDecoder();
    let output = "";

    const pump = async (stream: ReadableStream<Uint8Array>) => {
      for await (const chunk of stream) {
        const text = decoder.decode(chunk, { stream: true });
        output += text;
        for (const line of text.split("\n")) {
          if (line !== "") {
            Trace("| %s", line);
            if (this.task) this.task.appendConsoleOutput(line);
          }
        }
      }
    };

    await Promise.all([pump(child.stdout), pump(child.stderr)]);
    const status = await child.status;
    if (!status.success) {
      throw new Error(`command exited with code ${status.code}`);
    }
    return output;
  }

  /** Prepend entries from a GITHUB_PATH file onto the step PATH. */
  private applyPathFile(pathFile: string | undefined): void {
    if (!pathFile) return;
    try {
      const lines = Deno.readTextFileSync(pathFile)
        .split("\n").map((l) => l.trim()).filter((l) => l.length > 0);
      if (lines.length === 0) return;
      const existing = this.ctx.env["PATH"] ?? "";
      this.ctx.env["PATH"] = [...lines, existing].filter(Boolean).join(":");
    } catch {
      // ignore
    }
  }

  /** Parse GitHub Actions workflow command annotations from output. */
  private parseAnnotations(output: string): void {
    for (let line of output.split("\n")) {
      if (!line.startsWith("::")) continue;
      line = line.slice(2);
      const idx = line.indexOf("::");
      const levelAndParams = idx >= 0 ? line.slice(0, idx) : line;
      const message = idx >= 0 ? line.slice(idx + 2) : "";

      const spaceIdx = levelAndParams.indexOf(" ");
      const level = spaceIdx >= 0 ? levelAndParams.slice(0, spaceIdx) : levelAndParams;
      if (level !== "error" && level !== "warning" && level !== "notice") continue;

      const annotation: GitHubCheckSuiteAnnotation = {
        annotation_level: level,
        message,
        title: message,
        raw_details: "::" + line,
      };

      if (spaceIdx >= 0) {
        const params = new URLSearchParams(levelAndParams.slice(spaceIdx + 1).replace(/,/g, "&"));
        const file = params.get("file");
        if (file) annotation.path = file;
        const path = params.get("path");
        if (path) annotation.path = path;
        const title = params.get("title");
        if (title) annotation.title = title;
        const lineNum = params.get("line");
        if (lineNum && !isNaN(Number(lineNum))) {
          annotation.start_line = Number(lineNum);
          annotation.end_line = Number(lineNum);
        }
        const endLine = params.get("endLine");
        if (endLine && !isNaN(Number(endLine))) annotation.end_line = Number(endLine);
      }

      (this.ctx.annotations[level] ??= []).push(annotation);
    }
  }

  private createSuccessStatus(): PolicyEngineStatus {
    return {
      status: StatusComplete,
      detail: {
        id: "",
        exit_status: "success",
        outputs: Object.fromEntries(Object.entries(this.ctx.jobResults).map(([job, r]) => [job, r.outputs])),
        annotations: { ...this.ctx.annotations },
        cache: this.ctx.cache,
      },
      console_output: this.ctx.consoleOutput.join("\n"),
    };
  }

  private createErrorStatus(err: Error): PolicyEngineStatus {
    const annotations: Record<string, unknown> = { ...this.ctx.annotations };
    annotations["error"] = [err.message];
    return {
      status: StatusComplete,
      detail: {
        id: "",
        exit_status: "failure",
        outputs: {},
        annotations,
        cache: this.ctx.cache,
      },
      console_output: this.ctx.consoleOutput.join("\n"),
    };
  }
}

/** Parse cache JSON, falling back to `previous` on empty or invalid input. */
export function parseCache(content: string, previous: Cache): Cache {
  if (!content) return previous;
  try {
    const parsed = JSON.parse(content);
    return parsed && typeof parsed === "object" ? (parsed as Cache) : previous;
  } catch {
    return previous;
  }
}

// --- module-level helpers ---

/**
 * Recursively convert string "true"/"false" into real booleans so that
 * comparisons like `steps.x.outputs.y === true` behave as expected.
 */
export function convertBoolStrings(value: unknown): unknown {
  if (value === "true") return true;
  if (value === "false") return false;
  if (Array.isArray(value)) return value.map(convertBoolStrings);
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[k] = convertBoolStrings(v);
    return out;
  }
  return value;
}

/** Convert dot notation to bracket notation, skipping string literals. */
export function transformPropertyAccessors(jsCode: string): string {
  let result = "";
  let i = 0;
  while (i < jsCode.length) {
    const ch = jsCode[i];
    if (ch === '"' || ch === "'") {
      const quote = ch;
      result += ch;
      i++;
      while (i < jsCode.length) {
        result += jsCode[i];
        if (jsCode[i] === quote) {
          i++;
          break;
        }
        i++;
      }
    } else if (ch === ".") {
      result += "['";
      i++;
      const start = i;
      while (i < jsCode.length && /[A-Za-z0-9_\-]/.test(jsCode[i])) i++;
      result += jsCode.slice(start, i);
      result += "']";
    } else {
      result += ch;
      i++;
    }
  }
  return result;
}

/** Resolve a property path like "github.actor" from data. */
export function resolvePropertyPath(path: string, data: Record<string, unknown>): unknown {
  let current: unknown = data;
  for (const part of path.split(".")) {
    if (current && typeof current === "object" && !Array.isArray(current)) {
      current = (current as Record<string, unknown>)[part];
      if (current === undefined) return undefined;
    } else {
      return undefined;
    }
  }
  return current;
}

/** Parse GitHub Actions output format (key=value and key<<delimiter blocks). */
export function parseGitHubActionsOutputs(content: string): Record<string, string> {
  const outputs: Record<string, string> = {};
  let currentKey = "";
  let currentDelimiter = "";
  let currentValue = "";

  for (const line of content.split("\n")) {
    if (currentDelimiter !== "") {
      if (line.startsWith(currentDelimiter)) {
        outputs[currentKey] = currentValue.replace(/\n$/, "");
        currentKey = "";
        currentDelimiter = "";
        currentValue = "";
      } else {
        currentValue += line + "\n";
      }
    } else if (line.includes("<<") && currentKey === "") {
      const [k, d] = splitN(line, "<<", 2);
      currentKey = k.trim();
      currentDelimiter = d.trim();
    } else if (line.includes("=") && currentKey === "") {
      const [k, v] = splitN(line, "=", 2);
      outputs[k.trim()] = v ?? "";
    }
  }
  return outputs;
}

/** Build the argument list for a `deno run` invocation, quieting downloads. */
export function denoRunArgs(...args: string[]): string[] {
  const result = ["run"];
  if (Deno.env.get("DEBUG_DENO_PACKAGES") !== "1") result.push("--quiet");
  return [...result, ...args];
}

/** Split a string on `sep` into at most `n` parts (like Go's strings.SplitN). */
function splitN(s: string, sep: string, n: number): string[] {
  const parts = s.split(sep);
  if (parts.length <= n) return parts;
  return [...parts.slice(0, n - 1), parts.slice(n - 1).join(sep)];
}

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.stat(path);
    return true;
  } catch {
    return false;
  }
}

async function readTextOrEmpty(path: string | undefined): Promise<string> {
  if (!path) return "";
  try {
    return await Deno.readTextFile(path);
  } catch {
    return "";
  }
}

async function unzip(src: string, dest: string): Promise<void> {
  const cmd = new Deno.Command("unzip", { args: ["-q", "-o", src, "-d", dest] });
  const { success, code } = await cmd.output();
  if (!success) throw new Error(`unzip failed with code ${code}`);
}
