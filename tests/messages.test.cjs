const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const context = vm.createContext({});
vm.runInContext(fs.readFileSync('web/messages.js', 'utf8') + '\nthis.parse = MessageContent.parse; this.renderFileCitations = MessageContent.renderFileCitations;', context);
const parse = text => JSON.parse(JSON.stringify(context.parse(text)));

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
  vm.runInContext(app.slice(app.indexOf('function audioPath('),app.indexOf('document.addEventListener("click", async event => {',app.indexOf('function audioPath('))) + app.slice(app.indexOf('function md(src)'),app.indexOf('// ------------------------------------------------------------- thread meta')), renderer);
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
