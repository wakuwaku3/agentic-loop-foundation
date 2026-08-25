const id=()=>crypto.randomUUID();const json=v=>({headers:{"Content-Type":"application/json"},body:JSON.stringify(v)});const requestedBySuffix=v=>v&&v.requested_by&&v.requested_by.actor_type?" (requested by "+v.requested_by.actor_type+")":"";const refresh=()=>fetch("/v1/queue/summary").then(r=>r.json()).then(v=>document.getElementById("queue").textContent=JSON.stringify(v,null,2));document.getElementById("refresh").onclick=refresh;refresh();document.getElementById("capture").onsubmit=e=>{e.preventDefault();fetch("/v1/requirements",{method:"POST",...json({request_id:id(),text:document.getElementById("text").value})}).then(r=>r.json()).then(v=>document.getElementById("capture-status").textContent=v.requirement_id?"Captured"+requestedBySuffix(v):"Unable to capture")};document.getElementById("control").onsubmit=e=>{e.preventDefault();fetch("/v1/controls",{method:"POST",...json({request_id:id(),scope_kind:document.getElementById("scope").value,scope_value:document.getElementById("scope-value").value,mode:document.getElementById("mode").value})}).then(r=>r.json()).then(v=>document.getElementById("control-status").textContent=v.revision?"Control requested"+requestedBySuffix(v):"Unable to apply")};const repoList=()=>document.getElementById("repository-list");const repoStatus=()=>document.getElementById("repository-status");const locatorText=l=>l?l.forge+":"+l.host+"/"+l.owner+"/"+l.name+(l.default_branch?"@"+l.default_branch:""):"unobserved locator";const renderRepositories=v=>{const list=repoList();list.textContent="";const rows=(v&&v.repositories)||[];if(!rows.length){const li=document.createElement("li");li.className="muted";li.textContent="No Repository is registered.";list.appendChild(li);return;}rows.forEach(r=>{const li=document.createElement("li");const id=document.createElement("div");id.className="repo-id";id.textContent=r.repository_id;li.appendChild(id);const loc=document.createElement("div");loc.textContent=locatorText(r.locator)+" \u2014 "+r.status+" (v"+r.version+")";li.appendChild(loc);const e=r.executability||{state:"unobserved",reason:"the response carried no executability field"};const st=document.createElement("div");st.className="repo-state state-"+e.state;st.textContent="loop executable: "+(e.state==="executable"?"yes":"no")+" ["+e.state+"]"+(e.stale?" (stale observation)":"");li.appendChild(st);const why=document.createElement("p");why.className="repo-reason";why.textContent=e.reason||"no reason was reported";li.appendChild(why);list.appendChild(li);});};const refreshRepositories=()=>fetch("/v1/repositories").then(r=>r.json()).then(renderRepositories).catch(()=>{const list=repoList();list.textContent="";const li=document.createElement("li");li.className="muted";li.textContent="Unable to read repositories.";list.appendChild(li);});document.getElementById("repository-refresh").onclick=refreshRepositories;refreshRepositories();document.getElementById("repository").onsubmit=e=>{e.preventDefault();const body={request_id:id(),source_url:document.getElementById("source-url").value};const branch=document.getElementById("default-branch").value;if(branch){body.default_branch=branch;}fetch("/v1/repositories",{method:"POST",...json(body)}).then(r=>r.json()).then(v=>{repoStatus().textContent=v.repository_id?"Registered "+v.repository_id+requestedBySuffix(v):"Unable to register: "+(v.message||"unknown error");if(v.repository_id){refreshRepositories();}});};

// V2-066 release surface: self-contained additive block. Reads GET /v1/release/state
// and renders the assembled Preview version, the conditions that are not met, the
// rollback target and what this environment class cannot observe. It renders only
// what the response carries and never substitutes a plausible value for a missing one.
(function(){
  var el=function(i){return document.getElementById(i);};
  var setList=function(id,rows,empty){
    var list=el(id);if(!list){return;}list.textContent="";
    if(!rows||!rows.length){var li=document.createElement("li");li.className="muted";li.textContent=empty;list.appendChild(li);return;}
    rows.forEach(function(r){list.appendChild(r);});
  };
  var conditionRow=function(c){
    var li=document.createElement("li");
    var head=document.createElement("div");head.className="repo-state state-"+(c.state==="met"?"executable":(c.state==="not-observable-here"?"unobserved":"blocked"));
    head.textContent=(c.id||"unnamed condition")+" \u2014 "+(c.state||"unreported state");li.appendChild(head);
    var text=document.createElement("div");text.textContent=c.contract_text||"";li.appendChild(text);
    var why=document.createElement("p");why.className="repo-reason";why.textContent=c.reason||"no reason was reported";li.appendChild(why);
    var by=document.createElement("p");by.className="repo-reason";by.textContent="decided by: "+((c.decided_by||[]).join(", ")||"no source was reported");li.appendChild(by);
    (c.refusals||[]).forEach(function(f){
      var r=document.createElement("p");r.className="repo-reason";
      r.textContent="refusal ["+(f.kind||"unnamed kind")+(f.capability?" / "+f.capability:"")+"]: "+(f.reason||"no reason was reported");
      li.appendChild(r);
    });
    return li;
  };
  var render=function(v){
    var cand=(v&&v.candidate)||{};
    el("release-version").textContent=v&&v.release_version?("release "+v.release_version+" \u2014 bundle "+(cand.bundle_digest||"unreported")+" \u2014 assembled "+(v.assembled_at||"unreported")+" ("+(v.environment_class||"unreported environment class")+")"):"no release version was reported";
    el("release-promotable").textContent="promotable: "+(v&&v.promotable?"yes":"no")+(v&&v.version_source?" \u2014 "+v.version_source:"");
    var pending=((v&&v.conditions)||[]).filter(function(c){return c.state!=="met";}).map(conditionRow);
    setList("release-conditions",pending,"Every reported condition is met.");
    var route=(v&&v.route)||{};
    el("release-rollback").textContent=route.recorded?("rollback target: "+(route.rollback_target_digest||"none")+" \u2014 rollback available: "+(route.rollback_available?"yes":"no")+" \u2014 stable: "+(route.stable_digest||"none")+" \u2014 "+(route.source||"")):(route.note||"no route recorded");
    setList("release-not-observed",((v&&v.not_observed)||[]).map(function(n){var li=document.createElement("li");li.textContent=n;return li;}),"The response reported nothing as unobserved.");
    el("release-gaps").textContent=((v&&v.residual_gaps)||[]).join(" | ");
  };
  var failed=function(m){
    el("release-version").textContent=m;
    el("release-promotable").textContent="";
    setList("release-conditions",[],m);
    el("release-rollback").textContent=m;
    setList("release-not-observed",[],m);
    el("release-gaps").textContent="";
  };
  var refreshRelease=function(){
    return fetch("/v1/release/state").then(function(r){
      if(r.status===503){return r.json().then(function(b){failed("Release state is unavailable: "+(b.message||"this process was given no release source root."));});}
      if(!r.ok){failed("Unable to read release state.");return;}
      return r.json().then(render);
    }).catch(function(){failed("Unable to read release state.");});
  };
  var button=el("release-refresh");
  if(button){button.onclick=refreshRelease;}
  refreshRelease();
})();

// V2-071 repository-scoped Requirement backlog: self-contained additive block.
// It renders the Repository detail's repository-scoped Requirement Backlog
// count and the per-row Repository of each Requirement. Every value comes from
// the API; nothing is restated, defaulted or guessed here. An absent
// measurement is rendered as its own explicit state and reason, and a
// Requirement with no association is rendered as having none. The block loads
// no external asset and no library, starts no timer and no poll loop, and
// fetches only this origin's own relative paths.
(function(){
  var el=function(i){return document.getElementById(i);};
  var setList=function(id,items,empty){
    var list=el(id);if(!list){return;}
    list.textContent="";
    if(!items.length){var li=document.createElement("li");li.className="muted";li.textContent=empty;list.appendChild(li);return;}
    items.forEach(function(r){list.appendChild(r);});
  };
  var requirementRow=function(r){
    var li=document.createElement("li");
    var head=document.createElement("div");head.className="repo-id";
    head.textContent=(r.requirement_id||"unnamed requirement")+" — "+(r.status||"unreported status");
    li.appendChild(head);
    var assoc=document.createElement("div");
    assoc.className="repo-state state-"+(r.repository_id?"executable":"unobserved");
    assoc.textContent=r.repository_id?("repository: "+r.repository_id):"records no Repository association";
    li.appendChild(assoc);
    return li;
  };
  var renderBacklog=function(v){
    var b=(v&&v.requirement_backlog)||null;
    if(!b){el("backlog-state").textContent="the response carried no requirement_backlog";el("backlog-reason").textContent="";return;}
    if(b.state!=="measured"){
      el("backlog-state").textContent="repository-scoped backlog: "+(b.state||"unreported state")+" (no count was measured)";
    }else{
      var scope=b.installation_scope||{};
      el("backlog-state").textContent="repository-scoped Requirements: "+(b.truncated?"at least ":"")+b.requirement_count+
        " — installation-wide Requirements: "+(scope.requirements===undefined?"unreported":scope.requirements);
    }
    el("backlog-reason").textContent=b.reason||"no reason was reported";
  };
  var backlogFailed=function(m){el("backlog-state").textContent=m;el("backlog-reason").textContent="";};
  var loadRows=function(){
    return fetch("/v1/requirements").then(function(r){
      if(!r.ok){setList("backlog-rows",[],"Unable to read the Requirement list.");return;}
      return r.json().then(function(v){
        setList("backlog-rows",((v&&v.requirements)||[]).map(requirementRow),"No Requirement is captured.");
      });
    }).catch(function(){setList("backlog-rows",[],"Unable to read the Requirement list.");});
  };
  var loadBacklog=function(id){
    if(!id){backlogFailed("Name a Repository id to read its scoped backlog.");return Promise.resolve();}
    return fetch("/v1/repositories/"+encodeURIComponent(id)).then(function(r){
      if(r.status===404){backlogFailed("No Repository is registered under that id.");return;}
      if(!r.ok){backlogFailed("Unable to read that Repository.");return;}
      return r.json().then(renderBacklog);
    }).catch(function(){backlogFailed("Unable to read that Repository.");});
  };
  var form=el("backlog");
  if(form){form.onsubmit=function(e){e.preventDefault();loadBacklog((el("backlog-repository").value||"").trim());loadRows();};}
  loadRows();
})();
