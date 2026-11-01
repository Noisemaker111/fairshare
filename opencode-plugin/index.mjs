// OpenCode v2 server plugin: every shell call goes through `fairshare hook opencode`, the same
// thread shell Claude Code and Codex use. `fairshare install` copies this folder to
// ~/.config/opencode/plugins/fairshare, which OpenCode loads at start.
// If fairshare is missing or fails, the call runs unchanged.
import { spawnSync } from 'node:child_process';
import path from 'node:path';

// where `fairshare install` puts the copy every agent runs
const EXE = process.env.FAIRSHARE_EXE || path.join(process.env.LOCALAPPDATA || '', 'fairshare', 'bin', 'fairshare.exe');
const SHELL_TOOLS = /^(bash|shell)$/;
// OpenCode picks $SHELL first, then pwsh, powershell, Git Bash, cmd.
const shell = /bash|\/sh$|zsh/i.test(process.env.SHELL || '') || process.platform !== 'win32' ? 'bash' : 'pwsh';

export function handle(event, directory) {
  if (!SHELL_TOOLS.test(event.tool)) return;
  const input = event.input && typeof event.input === 'object' ? event.input : {};
  const field = typeof input.command === 'string' ? 'command' : typeof input.cmd === 'string' ? 'cmd' : null;
  if (!field) return;
  const payload = {
    hook_event_name: 'PreToolUse', tool_name: event.tool, session_id: event.sessionID,
    cwd: input.workdir || input.cwd || directory, tool_input: input, command_field: field,
  };
  const r = spawnSync(EXE, ['hook', 'opencode'], {
    input: JSON.stringify(payload), encoding: 'utf8', timeout: 10000, windowsHide: true,
    env: { ...process.env, FAIRSHARE_OPENCODE_SHELL: shell },
  });
  if (r.status !== 0 || !r.stdout) return;
  const out = JSON.parse(r.stdout).hookSpecificOutput || {};
  if (out.permissionDecision === 'deny') throw new Error(out.permissionDecisionReason);
  if (out.updatedInput) event.input = out.updatedInput;
}

export default {
  id: 'fairshare',
  async setup(host) {
    const directory = host?.location?.directory;
    await host.tool.hook('execute.before', (event) => {
      try { handle(event, directory); }
      catch (e) { if (String(e?.message).startsWith('[fairshare]')) throw e; }
    });
  },
};
