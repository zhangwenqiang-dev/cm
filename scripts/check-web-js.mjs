import fs from "node:fs";
import vm from "node:vm";

const indexURL = new URL("../web/index.html", import.meta.url);
const html = fs.readFileSync(indexURL, "utf8");
const workbenchURL = new URL("../web/assets/connectmac-workbench.js", import.meta.url);
const workbench = fs.readFileSync(workbenchURL, "utf8");
const scriptPattern = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;

let inlineScriptCount = 0;
let match;
while ((match = scriptPattern.exec(html)) !== null) {
  const attributes = match[1] || "";
  if (/\bsrc\s*=/i.test(attributes)) {
    continue;
  }

  inlineScriptCount++;
  try {
    new vm.Script(match[2], {
      filename: `web/index.html:inline-${inlineScriptCount}`,
    });
  } catch (error) {
    console.error(`web JavaScript syntax error in inline script ${inlineScriptCount}: ${error.message}`);
    process.exitCode = 1;
  }
}

if (inlineScriptCount === 0) {
  console.error("web JavaScript syntax check found no inline scripts");
  process.exitCode = 1;
}

try {
  new vm.Script(workbench, { filename: "web/assets/connectmac-workbench.js" });
} catch (error) {
  console.error(`web JavaScript syntax error in workbench asset: ${error.message}`);
  process.exitCode = 1;
}

if (inlineScriptCount > 0 && !process.exitCode) {
  console.log(`web JavaScript syntax OK (${inlineScriptCount} inline script)`);
}
