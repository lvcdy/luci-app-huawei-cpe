// 临时校验脚本：提取 luci view htm 中的 <script> 块，替换 LuCI 模板占位符后做语法检查。
// 用法: node scripts/check-luci-js.mjs <file.htm>...
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';

const files = process.argv.slice(2);
if (!files.length) { console.error('usage: node check-luci-js.mjs <file.htm>...'); process.exit(2); }

const dir = mkdtempSync(join(tmpdir(), 'lucijs-'));
let fail = 0;

for (const f of files) {
	const raw = readFileSync(f, 'utf8');
	const m = raw.match(/<script[^>]*>([\s\S]*?)<\/script>/i);
	if (!m) { console.log(`SKIP  ${f} (no script block)`); continue; }
	let code = m[1].replace(/^\/\/<!\[CDATA\[/, '').replace(/\/\/\]\]>$/m, '').trim();
	// LuCI 模板占位符替换为合法 JS 值
	code = code
		.replace(/<%=\s*url\([^)]*\)\s*%>/g, "'/admin/x'")
		.replace(/<%=\s*([^%]*?)\s*%>/g, "'X'")
		.replace(/<%:[^%]*%>/g, 'X')
		.replace(/<%\+[^%]*%>/g, '')
		.replace(/<%#[\s\S]*?%>/g, '');
	const tmp = join(dir, f.replace(/[\\\/:]/g, '_') + '.js');
	writeFileSync(tmp, code);
	const r = spawnSync(process.execPath, ['--check', tmp], { encoding: 'utf8' });
	if (r.status === 0) console.log(`OK    ${f}`);
	else { fail++; console.log(`FAIL  ${f}\n${r.stderr}`); }
}

rmSync(dir, { recursive: true, force: true });
process.exit(fail ? 1 : 0);
