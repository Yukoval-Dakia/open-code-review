// internal/impact/ts_refs.js
// Reads a JSON request on stdin, writes a JSON response on stdout.
// Request:  {mode:"symbols", content, changed:[lineNums]} |
//           {mode:"refs", content, name}
// Response: {symbols:[{name,kind,line}]} | {refs:[{line,kind}]} | {error}
// Resolves 'typescript' from the CWD's node_modules (the repo under review).
const chunks = [];
process.stdin.on('data', c => chunks.push(c));
process.stdin.on('end', () => {
  try {
    const ts = require('typescript');
    const req = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    const sf = ts.createSourceFile('f.tsx', req.content, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    const lineOf = pos => sf.getLineAndCharacterOfPosition(pos).line + 1;
    if (req.mode === 'symbols') {
      const changed = new Set(req.changed || []);
      const symbols = [];
      const kindFor = n => {
        if (ts.isFunctionDeclaration(n)) return 'function';
        if (ts.isMethodDeclaration(n)) return 'method';
        if (ts.isClassDeclaration(n)) return 'class';
        if (ts.isInterfaceDeclaration(n)) return 'interface';
        if (ts.isTypeAliasDeclaration(n)) return 'type';
        if (ts.isEnumDeclaration(n)) return 'enum';
        return null;
      };
      const visit = n => {
        const kind = kindFor(n);
        if (kind && n.name && ts.isIdentifier(n.name)) {
          const line = lineOf(n.name.getStart(sf));
          if (changed.has(line)) symbols.push({ name: n.name.text, kind, line });
        }
        ts.forEachChild(n, visit);
      };
      visit(sf);
      process.stdout.write(JSON.stringify({ symbols }));
    } else if (req.mode === 'refs') {
      const refs = [];
      const seen = new Set();
      const visit = n => {
        if (ts.isIdentifier(n) && n.text === req.name) {
          const line = lineOf(n.getStart(sf));
          if (!seen.has(line)) {
            seen.add(line);
            let kind = 'ref';
            const p = n.parent;
            if (p && ts.isCallExpression(p) && p.expression === n) kind = 'call';
            else if (p && (ts.isImportSpecifier(p) || ts.isImportClause(p))) kind = 'import';
            else if (p && ts.isTypeReferenceNode(p)) kind = 'type-use';
            refs.push({ line, kind });
          }
        }
        ts.forEachChild(n, visit);
      };
      visit(sf);
      process.stdout.write(JSON.stringify({ refs }));
    } else {
      process.stdout.write(JSON.stringify({ error: 'unknown mode' }));
    }
  } catch (e) {
    process.stdout.write(JSON.stringify({ error: String(e && e.message || e) }));
  }
});
