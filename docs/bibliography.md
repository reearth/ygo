# Bibliography — ygo XML wire-conformance fix

Sources reviewed, referenced, or learned from during development.

| Source | Date | Referenced For |
|--------|------|----------------|
| [yjs 13.6.31 dist source](/root/burrow/core/web/node_modules/yjs/dist/yjs.mjs) | 2026-07-10 | Reference wire semantics: `Item.getMissing` skips the missing-struct check for same-client parent refs (why forward parent refs crash `findIndexSS`); `UpdateEncoderV2.writeKey` has key dedup deliberately disabled upstream (commented-out `keyMap.set` with compat warning); `YXmlFragment._prelimContent` / `YXmlElement._prelimAttrs` / `YText._pending` detached-type buffering and `_integrate` flush order (children before attributes). |
| [y-prosemirror 1.3.7](/root/burrow/core/web/node_modules/y-prosemirror) | 2026-07-10 | The shapes Burrow's editor produces over `Y.XmlFragment("prosemirror")`: typed attribute values (heading `level` as a number), detached bottom-up node construction, marks as ContentFormat pairs. Drove fixture design. |
| [Burrow editor collab binding](/root/burrow/core/web) | 2026-07-10 | Read-only reference: `@milkdown/plugin-collab` binds `getXmlFragment('prosemirror')` — fixed the root type name used in all fixtures and the spike. |
| [ygo fidelity spike harness](/tmp/ygo-spike) | 2026-07-10 | Reproduction harness + captured yjs reference bytes for the 3 failures; acceptance gate for the fix (Tests 1/2/3). |
