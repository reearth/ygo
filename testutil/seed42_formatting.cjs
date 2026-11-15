const Y=require('yjs');
const fs=require('fs');
const scenarios=JSON.parse(fs.readFileSync(0,'utf8'));
const results=scenarios.map(s=>{
 const xmlMode=process.argv[2]==='xml';
 let base;
 if(xmlMode){const seed=new Y.Doc();seed.clientID=100;const p=new Y.XmlElement('paragraph');p.insert(0,[new Y.XmlText()]);seed.getXmlFragment('prosemirror').insert(0,[p]);base=Y.encodeStateAsUpdate(seed);}
 const docs=[1,2,3].map(id=>{const d=new Y.Doc();d.clientID=id;if(xmlMode){d.getXmlFragment('prosemirror');Y.applyUpdate(d,base);}else{d.getText('t');}return d;});
 const texts=docs.map(d=>xmlMode?d.getXmlFragment('prosemirror').get(0).get(0):d.getText('t'));
 const sync=(a,b)=>Y.applyUpdate(docs[b],Y.encodeStateAsUpdate(docs[a]));
 for(const st of s.steps){
  if(st.op==='sync'){sync(st.peer,st.to);continue;}
  const t=texts[st.peer],pos=Math.min(st.pos,t.length),n=Math.min(st.n||0,t.length-pos);
  docs[st.peer].transact(()=>{
   if(st.op==='insert')t.insert(pos,st.text,st.attrs||{});
   else if(st.op==='delete'&&n>0)t.delete(pos,n);
   else if(st.op==='format'&&n>0)t.format(pos,n,st.attrs||{});
  });
 }
 for(let r=0;r<2;r++)for(let a=0;a<3;a++)for(let b=0;b<3;b++)if(a!==b)sync(a,b);
 return texts.map(t=>t.toDelta().flatMap(d=>Array.from(d.insert).map(text=>({text,attrs:d.attributes||{}}))));
});
process.stdout.write(JSON.stringify(results));
