/// <reference lib="dom" />
import puppeteer from "npm:puppeteer-core@23";
import { parseArgs } from "jsr:@std/cli@1/parse-args";

const args = parseArgs(Deno.args, {
  string: ["creds", "account", "url", "out", "done", "chrome", "sign-in-as", "cookie-file", "chrome-arg", "click"],
  boolean: ["start-api"],
  collect: ["chrome-arg"],
});
const fail = (msg: string): never => {
  console.error(`oauth-signin: ${msg}`);
  Deno.exit(2);
};
if (!args.creds || !args.account || !args.url) fail("--creds, --account and --url are required");

const vars: Record<string, string> = {};
for (const line of (await Deno.readTextFile(args.creds!)).split("\n")) {
  const m = line.match(/^\s*(?:export\s+)?([A-Z0-9_]+)=(.*)$/);
  if (m) vars[m[1]] = m[2].replace(/^['"]|['"]$/g, "");
}
const handle = vars[`${args.account}_ATPROTO_HANDLE`] ?? fail(`${args.account}_ATPROTO_HANDLE is not in ${args.creds}`);
const password = vars[`${args.account}_ATPROTO_PASSWORD`] ?? fail(`${args.account}_ATPROTO_PASSWORD is not in ${args.creds}`);
const out = args.out ?? `${Deno.env.get("HOME")}/.tmp/oauth-signin/${new Date().toISOString().replace(/[:.]/g, "")}`;
await Deno.mkdir(out, { recursive: true, mode: 0o700 });
const origin = new URL(args.url!).origin;

const browser = await puppeteer.launch({
  executablePath: args.chrome ?? "/usr/bin/google-chrome",
  headless: true,
  args: ["--no-sandbox", "--disable-dev-shm-usage", ...((args["chrome-arg"] as string[] | undefined) ?? [])],
});
const page = await browser.newPage();
page.setDefaultTimeout(60_000);
let n = 0;
const step = async (name: string) => {
  n++;
  await page.screenshot({ path: `${out}/${String(n).padStart(2, "0")}-${name}.png` });
  console.log(JSON.stringify({ step: name, url: page.url().replace(/[?#].*/, "") }));
};
const pause = (ms: number) => new Promise((r) => setTimeout(r, ms));
const clickButton = (re: string) =>
  page.evaluate((src: string) => {
    const want = new RegExp(src, "i");
    const btn = [...document.querySelectorAll("button, a[role=button], input[type=submit]")]
      .find((b) => want.test(((b as HTMLElement).innerText || (b as HTMLInputElement).value || "").trim()));
    if (btn) {
      (btn as HTMLElement).click();
      return ((btn as HTMLElement).innerText || (btn as HTMLInputElement).value || "").trim();
    }
    return null;
  }, re);

try {
  if (args["start-api"]) {
    const res = await fetch(origin + "/oauth/start", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ handle }),
    });
    const body = await res.text();
    if (res.status !== 200) throw new Error(`oauth start answered ${res.status}: ${body}`);
    const cookie = res.headers.get("set-cookie")?.split(";")[0];
    if (!cookie) throw new Error("oauth start returned no state cookie");
    const eq = cookie.indexOf("=");
    await page.setCookie({ name: cookie.slice(0, eq), value: cookie.slice(eq + 1), url: origin + "/" });
    const authorizationUrl = JSON.parse(body).authorizationUrl;
    if (typeof authorizationUrl !== "string" || !authorizationUrl) throw new Error("oauth start returned no authorizationUrl");
    await page.goto(authorizationUrl, { waitUntil: "networkidle2" });
    await step("start");
  } else {
    await page.goto(args.url!, { waitUntil: "networkidle2" });
    await step("start");
  }

  const handleInput = await page.$(
    '#handle, input[name="handle"], input[placeholder*="handle" i], input[type="text"]:not([hidden]), input:not([type]):not([hidden]), input[type="url"], input[type="email"]',
  );
  if (handleInput && new URL(page.url()).origin === origin) {
    await handleInput.click();
    await handleInput.type((args["sign-in-as"] as string | undefined) ?? handle);
    await page.keyboard.press("Enter");
    await page.waitForNavigation({ waitUntil: "networkidle2" }).catch(() => {});
    await step("to-authorization-server");
  } else {
    const clicked = await clickButton("^(sign in|log ?in|continue with bluesky|connect)");
    if (clicked) {
      await page.waitForNavigation({ waitUntil: "networkidle2" }).catch(() => {});
      await step("to-authorization-server");
    }
  }

  await clickButton("^sign in$");
  await pause(2000);
  const ident = await page.$('input[type="text"], input[type="email"], input[autocomplete="username"]');
  if (ident) {
    const val = await page.evaluate((el) => (el as HTMLInputElement).value, ident);
    if (!val) await ident.type(handle);
  }
  const pw = await page.waitForSelector('input[type="password"]', { visible: true });
  await pw!.type(password);
  await step("credentials-entered");
  await page.keyboard.press("Enter");
  await pause(4000);
  await step("after-password");

  for (let i = 0; i < 6 && new URL(page.url()).origin !== origin; i++) {
    const clicked = await clickButton("^(accept|authorize|allow|continue|approve)$");
    console.log(JSON.stringify({ step: "consent", clicked }));
    await pause(4000);
    await step(`consent-${i}`);
  }
  if (new URL(page.url()).origin !== origin) throw new Error(`still on ${new URL(page.url()).origin} after consent`);
  await pause(3000);
  const cookies = (await page.cookies()).map((c) => ({ name: c.name, httpOnly: c.httpOnly, secure: c.secure, sameSite: c.sameSite, path: c.path }));
  console.log(JSON.stringify({ cookies }));
  if (args["cookie-file"]) {
    const jar = await page.cookies();
    await Deno.writeTextFile(args["cookie-file"] as string, JSON.stringify(jar), { mode: 0o600 });
  }
  if (args.click) {
    await page.waitForFunction((src: string) => {
      const want = new RegExp(src, "i");
      return [...document.querySelectorAll("button")].some((b) => want.test((b as HTMLElement).innerText.trim()) && !(b as HTMLButtonElement).disabled);
    }, { timeout: 60_000 }, args.click as string);
    console.log(JSON.stringify({ step: "click", clicked: await clickButton(args.click as string) }));
    await pause(4000);
    await step("clicked");
  }
  if (args.done) await page.waitForFunction((t: string) => document.body.innerText.includes(t), { timeout: 60_000 }, args.done);
  await step("done");
  console.log(JSON.stringify({ ok: true, screenshots: out }));
} catch (err) {
  await step("error").catch(() => {});
  console.error(JSON.stringify({ ok: false, error: String(err), screenshots: out }));
  await browser.close();
  Deno.exit(1);
}
await browser.close();
