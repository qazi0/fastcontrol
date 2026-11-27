const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const context = vm.createContext({});
vm.runInContext(fs.readFileSync('web/messages.js', 'utf8') + '\nthis.parse = MessageContent.parse; this.renderFileCitations = MessageContent.renderFileCitations; this.cleanUserMessage = MessageContent.cleanUserMessage; this.MessageContent = MessageContent;', context);
const parse = text => JSON.parse(JSON.stringify(context.parse(text)));
const heartbeat = '<heartbeat>\n<automation_id>internal-check-id</automation_id>\n<decision>NOTIFY</decision>\n<message>Final verification and scoped shutdown completed. **All records match.**\n\nReport: [Final Report](/reports/final report.md).</message>\n</heartbeat>';

test('question replies expose questions and answers without transport identifiers', () => {
  const replies = [{questionItemId:'private-call-id',question:'Which folder?',answer:'My reports'}, {question:'Continue?',answer:'Yes'}];
  assert.deepEqual(parse('<send_user_message_question_reply>\n' + JSON.stringify(replies) + '\n</send_user_message_question_reply>'), [
    {kind:'reply',question:'Which folder?',answer:'My reports'}, {kind:'reply',question:'Continue?',answer:'Yes'}
  ]);
});
test('injected instructions and session metadata become named context', () => {
  assert.deepEqual(parse('# AGENTS.md instructions\n\n<INSTRUCTIONS>\nUse clear names.\n</INSTRUCTIONS><environment_context>\n<cwd>/work</cwd>\n<shell>zsh</shell>\n<filesystem>private internals</filesystem>\n</environment_context>\nFix the bug.'), [
    {kind:'context',title:'Project Instructions',text:'Use clear names.'},
    {kind:'context',title:'Session Context',text:'Working Directory: /work\nShell: zsh'},
    {kind:'text',text:'Fix the bug.'}
  ]);
});
test('ordinary XML, inline examples, fenced examples and incomplete envelopes remain text', () => {
  for (const text of ['<invoice>hello</invoice>', 'Example: <send_user_message_question_reply>[]</send_user_message_question_reply>', '```xml\n<send_user_message_question_reply>[]</send_user_message_question_reply>\n```', '~~~\n<environment_context>x</environment_context>\n~~~', '<environment_context>still typing']) assert.deepEqual(parse(text), [{kind:'text',text}]);
});
test('bad reply payloads never expose raw control JSON', () => {
  for (const body of ['{broken', 'null', '{}', '[{"questionItemId":"private"}]']) assert.deepEqual(parse('<send_user_message_question_reply>' + body + '</send_user_message_question_reply>'), [{kind:'text',text:'Answered a question'}]);
});
test('scheduled checks and local commands hide protocol scaffolding', () => {
  assert.deepEqual(parse('<heartbeat><automation_id>private</automation_id><instructions>Check the build.</instructions></heartbeat>\n<command-name>/status</command-name>'), [
    {kind:'context',title:'Scheduled Check',text:'Check the build.'}, {kind:'context',title:'Command',text:'/status'}
  ]);
});
test('scheduled results retain their report and omit automation routing fields', () => {
  assert.deepEqual(parse(heartbeat),[{kind:'text',text:'Final verification and scoped shutdown completed. **All records match.**\n\nReport: [Final Report](/reports/final report.md).'}]);
  assert.deepEqual(parse('<heartbeat><automation_id>internal-check-id</automation_id><decision>SILENT</decision><message></message></heartbeat>'),[]);
  assert.deepEqual(parse('<heartbeat><automation_id>internal-check-id</automation_id><decision>SILENT</decision></heartbeat>'),[]);
});
test('streamed scheduled results never expose partial transport tags or IDs', () => {
  for (let n=1;n<=heartbeat.length;n++) {
    const result=JSON.stringify(context.parse(heartbeat.slice(0,n),0,true));
    assert.doesNotMatch(result,/<\/?|automation_id|internal-check-id|NOTIFY|decision>/, 'prefix '+n);
  }
  const literal='```xml\n'+heartbeat+'\n```';
  assert.deepEqual(parse(literal),[{kind:'text',text:literal}]);
  assert.deepEqual(parse('Example: `<heartbeat>hello</heartbeat>`'),[{kind:'text',text:'Example: `<heartbeat>hello</heartbeat>`'}]);
});
test('Claude task notices show the result and omit internal paths and IDs', () => {
  const result = parse('<system-reminder>\n<task-notification><task-id>private</task-id><output-file>/private/output</output-file><status>completed</status><summary>Checked the files</summary><result>Everything passed.</result></task-notification>\n</system-reminder>');
  assert.deepEqual(result, [{kind:'context',title:'Session Note',parts:[{kind:'context',title:'Task Completed',text:'Checked the files\n\nEverything passed.'}]}]);
});

test('file citations show filenames and line ranges without provider scaffolding', () => {
  const render = text => context.renderFileCitations(text, ({path,label}) => JSON.stringify({path,label}));
  assert.equal(render('Source: :codex-file-citation{path="/reports/Vision Capability Workbook.xlsx" artifact_kind="workbook" purpose="source"}'), 'Source: ' + JSON.stringify({path:'/reports/Vision Capability Workbook.xlsx',label:'Vision Capability Workbook.xlsx'}));
  assert.equal(render('::codex-file-citation{path="/src/app.ts" line_start=12 line_end="18"}'), JSON.stringify({path:'/src/app.ts',label:'app.ts:12-18'}));
  assert.equal(render(':codex-file-citation{path="C:\\\\Reports\\\\Budget.xlsx" start_line=2 end_line=5}'), JSON.stringify({path:'C:\\Reports\\Budget.xlsx',label:'Budget.xlsx:2-5'}));
  const example = '`::codex-file-citation{path="/example.txt"}`';
  assert.equal(render(example),example);
  assert.equal(render('``:codex-file-citation{path="/example.txt"}``'), '``:codex-file-citation{path="/example.txt"}``');
  for (const body of ['path="javascript:alert(1)"', 'path="relative.txt"', 'path="/a" path="/b"', 'purpose="source"', 'path="/a\\u0000b"']) assert.equal(render(':codex-file-citation{' + body + '}'), 'Source Unavailable');
});

test('Markdown file citations use escaped, host-scoped downloads and preserve literal examples', () => {
  const app = fs.readFileSync('web/app.js','utf8');
  const renderer = vm.createContext({
    S:{sid:'selected-host'}, I:{download:'<svg></svg>'},
    esc:s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    base:s => s.split(/[\\/]/).pop(),
    MessageContent:{renderFileCitations:context.renderFileCitations},
    isAudioPath:()=>false,
  });
  vm.runInContext(app.slice(app.indexOf('function audioPath('),app.indexOf('document.addEventListener("click", async event => {',app.indexOf('function audioPath('))) + app.slice(app.indexOf('function md('),app.indexOf('// ------------------------------------------------------------- thread meta')), renderer);
  const citation = ':codex-file-citation{path="/reports/Vision Capability Workbook.xlsx" artifact_kind="workbook" purpose="source"}';
  const html = renderer.md('Source: '+citation);
  assert.match(html,/class="file-download"/);
  assert.match(html,/data-file-server="selected-host"/);
  assert.match(html,/data-file-path="\/reports\/Vision Capability Workbook.xlsx"/);
  assert.match(html,/<span>Vision Capability Workbook.xlsx<\/span>/);
  assert.doesNotMatch(html,/codex-file-citation|artifact_kind|purpose=/);
  assert.match(renderer.md('```text\n'+citation+'\n```'),/codex-file-citation/);
  assert.doesNotMatch(renderer.md('```text\n'+citation+'\n```'),/file-download/);
  assert.doesNotMatch(renderer.md('`'+citation+'`'),/file-download/);
  const unsafe = renderer.md(':codex-file-citation{path="/reports/<img onerror=alert(1)>.txt"}');
  assert.doesNotMatch(unsafe,/<img/);
  assert.match(unsafe,/&lt;img onerror=alert\(1\)&gt;/);
  assert.match(renderer.md(citation+' and '+citation),/file-download[\s\S]*file-download/);
});

test('assistant responses and user messages share envelope formatting without losing Markdown', () => {
  const app=fs.readFileSync('web/app.js','utf8');
  const renderer=vm.createContext({
    S:{sid:'selected-host'}, I:{download:'<svg></svg>'},
    esc:s=>String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    base:s=>s.split(/[\\/]/).pop(),MessageContent:context.MessageContent || {parse:context.parse,renderFileCitations:context.renderFileCitations},isAudioPath:()=>false,
  });
  vm.runInContext(app.slice(app.indexOf('function audioPath('),app.indexOf('document.addEventListener("click", async event => {',app.indexOf('function audioPath(')))+app.slice(app.indexOf('function md('),app.indexOf('// ------------------------------------------------------------- thread meta'))+app.slice(app.indexOf('function messageHTML('),app.indexOf('function turnHTML(')),renderer);
  const html=renderer.messageHTML(heartbeat,true);
  assert.match(html,/<b>All records match\.<\/b>/);
  assert.match(html,/data-file-path="\/reports\/final report.md"/);
  assert.match(html,/<span>Final Report<\/span>/);
  assert.doesNotMatch(html,/heartbeat|automation_id|internal-check-id|NOTIFY|&lt;\/?message/);
  assert.match(renderer.messageHTML('<system-reminder>\n**Session note**\n</system-reminder>',true),/<b>Session note<\/b>/);
  assert.match(renderer.messageHTML('<send_user_message_question_reply>[{"question":"Folder?","answer":"Reports"}]</send_user_message_question_reply>',true),/answered-question/);
  assert.match(renderer.messageHTML('<invoice>hello</invoice>',true),/&lt;invoice&gt;hello&lt;\/invoice&gt;/);
  assert.match(renderer.messageHTML('```xml\n'+heartbeat+'\n```',true),/&lt;heartbeat&gt;/);
  assert.match(renderer.messageHTML('Hello **world**',false),/Hello \*\*world\*\*/);
  const paint=app.slice(app.indexOf('function paintResponse('),app.indexOf('function revealResponses('));
  assert.match(paint,/content\.innerHTML = messageHTML\(r\.shown, true,/);
  assert.doesNotMatch(paint,/content\.innerHTML = md\(/);
});

test('Antigravity XML envelopes and BEGIN/END CHAT markers are stripped while preserving code fences and Codex/Claude envelopes', () => {
  const wrapped = '<USER_REQUEST>\nFix the build error\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-27T20:00:00Z\n<SKILL>\nname: build-helper\n</SKILL>\n</ADDITIONAL_METADATA>\n<USER_SETTINGS_CHANGE>\nmodel=gemini-4-neon\n</USER_SETTINGS_CHANGE>';
  assert.equal(context.cleanUserMessage(wrapped), 'Fix the build error');
  assert.deepEqual(parse(wrapped), [{kind:'text', text:'Fix the build error'}]);

  // Truncated preview / unclosed tag + partial closing tag.
  assert.equal(context.cleanUserMessage('<USER_REQUEST>\nTruncated prompt text'), 'Truncated prompt text');
  assert.equal(context.cleanUserMessage('<USER_REQUEST>\nTruncated prompt</USER_REQU'), 'Truncated prompt');
  assert.equal(context.cleanUserMessage('<USER_REQUEST>\nDone\n</USER_REQUEST>\n<ADDITIONAL_ME\u2026'), 'Done');

  // BEGIN CHAT / END CHAT variants.
  assert.equal(context.cleanUserMessage('BEGIN CHAT\nCheck tests\nEND CHAT\n<ADDITIONAL_METADATA>time</ADDITIONAL_METADATA>'), 'Check tests');
  assert.equal(context.cleanUserMessage('--- BEGIN CHAT ---\n<begin_chat>\n<USER_REQUEST>\nRun checks\n</USER_REQUEST>\n<end_chat>\n--- END CHAT ---'), 'Run checks');
  assert.equal(context.cleanUserMessage('<begin_chat>Inline prompt<end_chat>'), 'Inline prompt');

  // Multiple context/metadata blocks.
  const multiMeta = '<ATTACHED_FILES>a.go</ATTACHED_FILES>\n<CURRENT_EDITOR>b.go</CURRENT_EDITOR>\n<OPEN_FILES>c.go</OPEN_FILES>\n<USER_CONTEXT>ctx</USER_CONTEXT>\n<CONTEXT>more</CONTEXT>\n<EPHEMERAL_MESSAGE>note</EPHEMERAL_MESSAGE>\nClean message';
  assert.equal(context.cleanUserMessage(multiMeta), 'Clean message');

  // System-only / metadata-only wakeups clean to empty string and empty parse parts.
  const sysWake = 'The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n<SYSTEM_MESSAGE>\n[Message] timer fired\n</SYSTEM_MESSAGE>';
  assert.equal(context.cleanUserMessage(sysWake), '');
  assert.deepEqual(parse(sysWake), []);
  assert.equal(context.cleanUserMessage('<USER_SETTINGS_CHANGE>effort=high</USER_SETTINGS_CHANGE>'), '');

  // Streaming user_message_chunk never flashes raw wrapper tag prefixes.
  for (let n = 1; n <= wrapped.length; n++) {
    const cleaned = context.cleanUserMessage(wrapped.slice(0, n), true);
    assert.doesNotMatch(cleaned, /<\/?(?:USER_REQUEST|ADDITIONAL_METADATA|USER_SETTINGS_CHANGE|SKILL)/i, 'prefix ' + n);
  }

  // Fenced code blocks, inline code, and plain prose MUST be preserved.
  const fencedUnwrapped = 'Example:\n```xml\n<USER_REQUEST>literal</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>\nBEGIN CHAT\nhi\nEND CHAT\n```';
  assert.equal(context.cleanUserMessage(fencedUnwrapped), fencedUnwrapped);
  const fencedWrapped = '<USER_REQUEST>\n' + fencedUnwrapped + '\n</USER_REQUEST>\n<ADDITIONAL_METADATA>real meta</ADDITIONAL_METADATA>';
  assert.equal(context.cleanUserMessage(fencedWrapped), fencedUnwrapped);
  assert.equal(context.cleanUserMessage('<USER_REQUEST>\nKeep `<USER_REQUEST>` and `</USER_REQUEST>` in <div>html</div>.\n</USER_REQUEST>'), 'Keep `<USER_REQUEST>` and `</USER_REQUEST>` in <div>html</div>.');
  assert.equal(context.cleanUserMessage('Let us begin chat testing and end chat logging.'), 'Let us begin chat testing and end chat logging.');
});

test('turnHTML cleans Antigravity user bubbles and omits system-only wakeup bubbles', () => {
  const app = fs.readFileSync('web/app.js', 'utf8');
  const renderer = vm.createContext({
    S: { sid: 'm1', open: new Set() },
    I: { chev: '>' },
    esc: s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    base: s => s.split(/[\\/]/).pop(),
    dur: ms => ms + 'ms',
    MessageContent: context.MessageContent,
    imagesHTML: () => '',
    audioHTMLs: () => '',
    entryHTML: html => html,
    stepHTML: () => '',
    responseState: (t, it, i) => ({ key: t.key + ':r:' + i }),
    md: s => `<p>${s}</p>`,
    Date,
  });
  vm.runInContext(
    app.slice(app.indexOf('function hiddenTool('), app.indexOf('function outputLines(')),
    renderer
  );
  const html = renderer.turnHTML({
    key: 'step-0',
    status: 'completed',
    items: [
      { id: 'u0', k: 'user', text: '<SYSTEM_MESSAGE>\nwakeup\n</SYSTEM_MESSAGE>' },
      { id: 'u1', k: 'user', text: '<USER_REQUEST>\nShip v0.2.19\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>' },
    ],
  });
  assert.match(html, /Ship v0\.2\.19/);
  assert.doesNotMatch(html, /USER_REQUEST|ADDITIONAL_METADATA|SYSTEM_MESSAGE|wakeup/);
  // Only one .u bubble rendered (the system-only wakeup was skipped, and the real user bubble is not marked steer).
  assert.equal((html.match(/class="u/g) || []).length, 1);
  assert.doesNotMatch(html, /class="u steer"/);
});
