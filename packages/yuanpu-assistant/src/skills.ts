import { lstat, readdir, realpath } from 'node:fs/promises';
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { parse } from 'yaml';
import { type AssistantHomePaths, readAssistantHomeFile } from './home.js';

export interface AssistantSkill {
  name: string;
  description: string;
  content: string;
  filePath: string;
}

export function formatAssistantSkills(skills: readonly AssistantSkill[]): string {
  const escape = (value: string) => value.replaceAll('&', '&amp;').replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;').replaceAll('"', '&quot;');
  return 'The following skills provide specialized instructions for specific tasks.\n<available_skills>\n'
    + skills.map((skill) => `<skill><name>${escape(skill.name)}</name><description>${escape(skill.description)}</description></skill>`).join('\n')
    + '\n</available_skills>';
}

function isInside(root: string, path: string): boolean {
  const child = relative(root, path);
  return child !== '..' && !child.startsWith(`..${sep}`) && !isAbsolute(child);
}

async function rejectSymlinks(path: string, root: string): Promise<void> {
  const info = await lstat(path);
  if (info.isSymbolicLink()) throw new Error(`Assistant skill symlink is forbidden: ${path}`);
  if (!isInside(root, await realpath(path))) throw new Error(`Assistant skill escapes skill root: ${path}`);
  if (info.isDirectory()) {
    for (const entry of await readdir(path)) await rejectSymlinks(join(path, entry), root);
  } else if (!info.isFile()) {
    throw new Error(`Unsupported assistant skill entry: ${path}`);
  }
}

function metadata(content: string, directoryName: string): { name: string; description: string } {
  const match = /^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/.exec(content);
  if (!match) throw new Error(`Missing SKILL.md frontmatter: ${directoryName}`);
  const parsed: unknown = parse(match[1] ?? '');
  const details = parsed && typeof parsed === 'object' && !Array.isArray(parsed)
    ? parsed as Record<string, unknown>
    : {};
  const name = details.name;
  const description = details.description;
  if (typeof name !== 'string' || name !== directoryName || !/^[a-z][a-z0-9-]{0,63}$/.test(name)
    || typeof description !== 'string' || !description.trim()) {
    throw new Error(`Invalid assistant skill metadata: ${directoryName}`);
  }
  return { name, description: description.trim() };
}

function rejectEscapingLinks(content: string, skillFile: string, skillRoot: string): void {
  for (const match of content.matchAll(/\]\(([^)]+)\)/g)) {
    const raw = match[1]?.trim().split(/\s+/, 1)[0];
    if (!raw || raw.startsWith('#') || /^(?:https?:|mailto:)/i.test(raw)) continue;
    if (/^[a-z][a-z0-9+.-]*:/i.test(raw)) {
      throw new Error(`Unsupported assistant skill reference: ${skillFile}`);
    }
    let path: string;
    try {
      path = decodeURIComponent(raw.split('#', 1)[0] ?? '');
    } catch {
      throw new Error(`Invalid assistant skill reference: ${skillFile}`);
    }
    if (!path || isAbsolute(path) || !isInside(skillRoot, resolve(dirname(skillFile), path))) {
      throw new Error(`Assistant skill reference escapes skill root: ${skillFile}`);
    }
  }
}

/** Read only the explicit assistant skill root. No Pi default/project/global discovery is invoked. */
export async function loadAssistantSkills(paths: AssistantHomePaths): Promise<AssistantSkill[]> {
  const canonicalRoot = await realpath(paths.skills);
  await rejectSymlinks(paths.skills, canonicalRoot);
  const skills: AssistantSkill[] = [];
  for (const entry of await readdir(paths.skills, { withFileTypes: true })) {
    if (entry.name.startsWith('.')) continue;
    if (!entry.isDirectory()) throw new Error(`Unexpected assistant skill root entry: ${entry.name}`);
    const filePath = join(paths.skills, entry.name, 'SKILL.md');
    const content = await readAssistantHomeFile(paths, filePath);
    const { name, description } = metadata(content, entry.name);
    rejectEscapingLinks(content, filePath, paths.skills);
    skills.push({ name, description, content, filePath });
  }
  return skills.sort((a, b) => a.name.localeCompare(b.name));
}
