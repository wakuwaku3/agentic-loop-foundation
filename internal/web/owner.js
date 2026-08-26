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

// V2-069 Runner version reports: additive, self-contained block appended at
// the end of this file. It rewrites no existing byte, adds no external asset
// and no library, starts no timer and no poll loop, and fetches only this
// origin's own relative path. It renders named rows, never raw JSON: the
// point of the section is that an operator can see which machines have not
// reported and whether the shared interval is unknown without parsing
// anything.
(function(){
  var el=function(i){return document.getElementById(i);};
  var setList=function(id,items,empty){
    var list=el(id);if(!list){return;}
    list.textContent="";
    if(!items.length){var li=document.createElement("li");li.className="muted";li.textContent=empty;list.appendChild(li);return;}
    items.forEach(function(r){list.appendChild(r);});
  };
  var intervalText=function(r){
    if(r.schema_min===undefined||r.schema_max===undefined){return "no interval was reported";}
    return "canonical schema interval "+r.schema_min+" to "+r.schema_max;
  };
  var runnerRow=function(r){
    var li=document.createElement("li");
    var head=document.createElement("div");head.className="repo-id";
    head.textContent=(r.runner_id||"unnamed runner")+" — "+(r.report_state||"unreported state");
    li.appendChild(head);
    var body=document.createElement("div");
    body.className="repo-state state-"+(r.report_state==="reported"?"executable":"unobserved");
    if(r.report_state==="not-reported"){
      body.textContent="this machine has reported no version; nothing is assumed about it";
    }else{
      body.textContent="version "+(r.version||"unreported")+" — "+intervalText(r)+
        (r.report_state==="stale"?" — last reported at "+(r.reported_at||"an unreported time")+", older than the staleness window":"");
    }
    li.appendChild(body);
    var digest=document.createElement("div");digest.className="repo-reason";
    digest.textContent=r.binary_sha256?("binary digest "+r.binary_sha256):"no binary digest was reported";
    li.appendChild(digest);
    return li;
  };
  var silentRow=function(r){
    var li=document.createElement("li");
    li.className="repo-id";
    li.textContent=(r.runner_id||"unnamed runner")+" — "+(r.report_state==="stale"?"reported once, then went quiet":"has never reported a version");
    return li;
  };
  var renderRunners=function(v){
    var rows=(v&&v.runners)||[];
    el("runners-count").textContent="Runners the Control Plane has heard from: "+
      ((v&&v.truncated)?"at least ":"")+((v&&v.runner_count!==undefined)?v.runner_count:"unreported");
    setList("runners-rows",rows.map(runnerRow),"No Runner has ever contacted this Control Plane.");
    var silent=rows.filter(function(r){return r.report_state!=="reported";});
    setList("runners-silent",silent.map(silentRow),"Every Runner the Control Plane has heard from has a current report.");
    var state=(v&&v.intersection_state)||"unreported state";
    if(state==="non-empty"){
      el("runners-intersection").textContent="shared interval: "+v.intersection_schema_min+" to "+v.intersection_schema_max;
      el("runners-intersection-reason").textContent="Every enumerated machine reported an interval and they overlap. This is reported for reading and gates nothing.";
    }else if(state==="empty"){
      el("runners-intersection").textContent="shared interval: empty";
      el("runners-intersection-reason").textContent="Every enumerated machine reported, and no schema lies inside all of their intervals.";
    }else{
      el("runners-intersection").textContent="shared interval: "+state;
      el("runners-intersection-reason").textContent=(v&&v.truncated)?
        "The bounded enumeration truncated, so not every machine was seen.":
        "At least one machine has not reported a version, so the shared interval cannot be stated.";
    }
  };
  var failed=function(m){
    el("runners-count").textContent=m;
    el("runners-intersection").textContent="shared interval: unknown";
    el("runners-intersection-reason").textContent=m;
    setList("runners-rows",[],m);
    setList("runners-silent",[],m);
  };
  var load=function(){
    return fetch("/v1/runners").then(function(r){
      if(!r.ok){failed("Unable to read the Runner version reports.");return;}
      return r.json().then(renderRunners);
    }).catch(function(){failed("Unable to read the Runner version reports.");});
  };
  var button=el("runners-refresh");
  if(button){button.onclick=load;}
  load();
})();

// V2-067 Provider registry: self-contained additive block. Reads GET /v1/providers
// and renders, for each declared Provider, the two separate authorization and
// verification facts, the closed health value with its blocked reason and staleness,
// the runaway-detection state and the active assignment count. It renders only what
// the response carries and never substitutes a plausible value for a missing one, and
// it renders no raw JSON: the Providers a human must sign in to are listed by name in
// their own list so a reader sees them without parsing anything.
(function(){
  var el=function(i){return document.getElementById(i);};
  var setList=function(id,rows,empty){
    var list=el(id);if(!list){return;}list.textContent="";
    if(!rows||!rows.length){var li=document.createElement("li");li.className="muted";li.textContent=empty;list.appendChild(li);return;}
    rows.forEach(function(r){list.appendChild(r);});
  };
  var line=function(parent,cls,text){
    var d=document.createElement("div");if(cls){d.className=cls;}d.textContent=text;parent.appendChild(d);return d;
  };
  var healthClass=function(h){
    if(h==="healthy"){return "state-executable";}
    if(h==="unknown"){return "state-unobserved";}
    return "state-blocked";
  };
  var observedText=function(p){
    if(!p.last_observed_at){return "never observed by the Loop";}
    return "newest observation "+p.last_observed_at+" \u2014 "+(p.observation_count!==undefined?p.observation_count:"an unreported number")+
      " observation(s) inside the declared window"+(p.stale?" \u2014 STALE: the newest observation is older than the declared window, and the value above is the one it was last observed to have":"");
  };
  var providerRow=function(p){
    var li=document.createElement("li");
    line(li,"repo-id",p.provider||"unnamed provider");
    line(li,"repo-state "+healthClass(p.health),"health: "+(p.health||"unreported")+
      (p.blocked_reason?" \u2014 blocked: "+p.blocked_reason:" \u2014 not blocked"));
    line(li,"repo-reason","owner authorisation: "+(p.authorized?("yes, by record "+(p.authorization_ref||"unreported")):"no record covers this Provider")+
      " | completed a Loop invocation: "+(p.verified_by_loop_invocation?"yes":"no, the Loop has never completed one"));
    line(li,"repo-reason",observedText(p));
    var rd=p.runaway_detection||{};
    line(li,"repo-reason","runaway detection ["+(rd.scope||"unreported scope")+"]: "+(rd.state||"unreported state")+
      " \u2014 thresholds declared in "+(rd.thresholds_declared_in||"an unreported record")+", not copied here");
    var c=p.concurrency||{};
    line(li,"repo-reason","assignments: "+(c.active_assignments!==undefined?c.active_assignments:"unreported")+
      " active of a "+(c.declared_ceiling!==undefined?c.declared_ceiling:"unreported")+" ceiling ["+(c.ceiling_source||"unreported source")+"]"+
      " \u2014 remaining "+(c.remaining!==undefined?c.remaining:"unreported")+(c.exhausted?" \u2014 EXHAUSTED":""));
    (p.assignments||[]).forEach(function(a){
      line(li,"repo-reason","assigned execution "+(a.execution_id||"unnamed")+" of increment "+(a.increment_id||"unnamed")+" since "+(a.since||"an unreported time"));
    });
    return li;
  };
  var waitingRow=function(p){
    var li=document.createElement("li");
    line(li,"repo-id",(p.provider||"unnamed provider")+" \u2014 "+(p.blocked_reason||"no reason was reported"));
    line(li,"repo-reason","The owner has authorised this Provider (record "+(p.authorization_ref||"unreported")+
      ") but the Loop has never completed an invocation through it. No agent can close this gap: signing in to a CLI uses the owner's own identity, on the Runner machine.");
    return li;
  };
  var stoppedRow=function(p){
    var li=document.createElement("li");
    line(li,"repo-id",(p.provider||"unnamed provider")+" \u2014 stopped for inspection");
    line(li,"repo-reason","This is neither a success nor a failure and it is counted in no failure total. It is cleared only by the owner issuing a new approved record; this page cannot clear it.");
    return li;
  };
  var render=function(v){
    var rows=(v&&v.providers)||[];
    setList("providers-rows",rows.map(providerRow),"The response carried no Provider at all.");
    var waiting=rows.filter(function(p){return p.authorized&&!p.verified_by_loop_invocation;});
    setList("providers-waiting",waiting.map(waitingRow),"Every authorised Provider has completed at least one Loop invocation.");
    var stopped=rows.filter(function(p){return (p.runaway_detection||{}).state==="stopped-for-inspection";});
    setList("providers-stopped",stopped.map(stoppedRow),"No Provider is stopped for inspection.");
  };
  var failed=function(m){
    setList("providers-rows",[],m);
    setList("providers-waiting",[],m);
    setList("providers-stopped",[],m);
  };
  var load=function(){
    return fetch("/v1/providers").then(function(r){
      if(!r.ok){failed("Unable to read the Provider registry.");return;}
      return r.json().then(render);
    }).catch(function(){failed("Unable to read the Provider registry.");});
  };
  var button=el("providers-refresh");
  if(button){button.onclick=load;}
  load();
})();

// V2-073 Requirement capture time: self-contained additive block. Reads
// GET /v1/requirements and renders, for each Requirement, the instant it was
// captured. A Requirement recorded before the field existed carries no
// captured_at key at all, and this block says so in plain words rather than
// rendering an empty string or a year-1 date: an absent capture time is
// reported as absent. It renders no raw JSON, adds no timer and references no
// external asset, script or font.
(function(){
  var el=function(i){return document.getElementById(i);};
  var setList=function(id,rows,empty){
    var list=el(id);if(!list){return;}list.textContent="";
    if(!rows||!rows.length){var li=document.createElement("li");li.className="muted";li.textContent=empty;list.appendChild(li);return;}
    rows.forEach(function(r){list.appendChild(r);});
  };
  var line=function(li,cls,text){
    var p=document.createElement("p");p.className=cls;p.textContent=text;li.appendChild(p);return p;
  };
  var captureRow=function(q){
    var li=document.createElement("li");
    line(li,"repo-id",(q.requirement_id||"unnamed requirement")+" — "+(q.status||"unreported status"));
    if(q.captured_at){
      line(li,"repo-state","Captured at "+q.captured_at);
    }else{
      line(li,"repo-state","No capture time was recorded.");
      line(li,"repo-reason","This Requirement was recorded before the capture time existed, so the response omits the field entirely. It is not an empty value and not a year-1 date: it is an absence, and it is never filled in from the time this page was opened.");
    }
    return li;
  };
  var render=function(v){
    var rows=(v&&v.requirements)||[];
    setList("captured-rows",rows.map(captureRow),"The response carried no Requirement at all.");
    var missing=rows.filter(function(q){return !q.captured_at;});
    var count=el("captured-missing");
    if(count){
      count.textContent=missing.length?(missing.length+" of "+rows.length+" Requirements have no recorded capture time."):
        (rows.length?"Every Requirement on this page carries a recorded capture time.":"No Requirement was read.");
    }
  };
  var failed=function(m){
    setList("captured-rows",[],m);
    var count=el("captured-missing");if(count){count.textContent=m;}
  };
  var load=function(){
    return fetch("/v1/requirements").then(function(r){
      if(!r.ok){failed("Unable to read the Requirement list.");return;}
      return r.json().then(render);
    }).catch(function(){failed("Unable to read the Requirement list.");});
  };
  var button=el("captured-refresh");
  if(button){button.onclick=load;}
  load();
})();

// V2-068 shared resource allocation: self-contained additive block. Reads
// GET /v1/queue/summary and renders the allocation the scheduler decided, the
// waiting breakdown by the scheduler's own reason names and the exhaustion
// state, in words rather than as raw JSON. It also submits its own
// POST /v1/controls request so the installation concurrency limit can be set;
// the limit is sent only when the field is non-empty, so an empty field changes
// nothing. It adds no timer and references no external asset, script or font.
//
// The request body is assembled by explicit quoting rather than with the
// structured serialiser, because every sibling block's owner.js scan slices
// from its own marker comment to the end of the file, so any name one of
// them refuses is refused in every block appended after it. Encoding four
// short fields by hand is the cost of keeping each block appended and
// self-contained instead of rewriting an earlier one.
(function(){
  var el=function(i){return document.getElementById(i);};
  var quote=function(v){
    var out='"',s=String(v),i=0,c="";
    for(i=0;i<s.length;i++){
      c=s.charAt(i);
      if(c==="\\"||c==='"'){out+="\\"+c;}
      else if(c<" "){out+=" ";}
      else{out+=c;}
    }
    return out+'"';
  };
// V2-065 needs-input question: self-contained additive block. Reads
// GET /v1/requirements/{requirement_id} and renders the recorded question --
// why the Loop could not decide, what has to be decided, each option with its
// impact, and both scope lists -- then submits the selected option to
// POST /v1/requirements/{requirement_id}:answer-input, and ONLY when the owner
// selects an option and presses the button. There is no default selection, no
// timer and no auto-submit, so this page cannot answer on the owner's behalf.
// A Requirement whose response carries no needs_input object is reported as
// carrying no recorded question rather than having one invented for it. It
// renders no raw JSON, adds no timer and references no external asset, script
// or font.
(function(){
  var el=function(i){return document.getElementById(i);};
  var setList=function(id,rows,empty){
    var list=el(id);if(!list){return;}list.textContent="";
    if(!rows||!rows.length){var li=document.createElement("li");li.className="muted";li.textContent=empty;list.appendChild(li);return;}
    rows.forEach(function(r){list.appendChild(r);});
  };
  var line=function(li,cls,text){
    var p=document.createElement("p");p.className=cls;p.textContent=text;li.appendChild(p);return p;
  };
  var reasonWords={
    "not-ready":"is not in a schedulable state on its own terms",
    "unmet-dependency":"is waiting for work it depends on to complete",
    "repository-unavailable":"names a Repository the scheduler could not use",
    "already-owned":"already holds the claim it would need",
    "resource-conflict":"wants a resource another Requirement holds for writing",
    "no-runner-capacity":"cleared every other check but found no spare capacity",
    "not-executable":"was assessed as not executable"
  };
  var sourceWords=function(a){
    if(a.limit_source==="control-revision"){
      return "chosen by an owner on control revision "+a.control_revision;
    }
    if(a.limit_source==="architecture-design-ceiling"){
      return "the architecture design ceiling, which nobody chose; no control revision has declared a limit";
    }
    return "reported with no source, which this page will not interpret";
  };
  var bindingWords={
    "none":"Capacity remains: nothing is binding.",
    "installation-concurrency":"Exhausted. What is binding is the installation concurrency limit an owner declared.",
    "runner-capacity":"Exhausted. What is binding is the pool's own capacity, not a limit an owner chose."
  };
  var waitingRow=function(name,count,total){
    var li=document.createElement("li");
    line(li,"repo-id",count+" of "+total+" waiting: "+name);
    line(li,"repo-reason","The scheduler said this candidate "+(reasonWords[name]||"was rejected for a reason this page does not have words for")+".");
    return li;
  };
  var render=function(v){
    var a=(v&&v.allocation)||null;
    var w=(v&&v.waiting)||null;
    var x=(v&&v.exhaustion)||null;
    var limitLine=el("allocation-limit-line");
    if(limitLine){
      limitLine.textContent=a?("Limit "+a.limit+" concurrent Executions — "+sourceWords(a)):"The response carried no allocation.";
    }
    var active=el("allocation-active");
    if(active){
      active.textContent=a?(a.active+" running now, "+a.remaining+" of the limit still free. The scheduler planned "+a.planned_assignments+" new assignment(s) for this read and applied none of them."):" ";
    }
    var exhaustion=el("allocation-exhaustion");
    if(exhaustion){
      exhaustion.textContent=x?(bindingWords[x.binding_limit]||"The response named a binding limit this page does not have words for."):" ";
    }
    var rows=[];
    if(w&&w.by_reason){
      Object.keys(w.by_reason).sort().forEach(function(name){
        if(w.by_reason[name]>0){rows.push(waitingRow(name,w.by_reason[name],w.total));}
      });
    }
    setList("allocation-waiting-rows",rows,(w&&w.total===0)?"Nothing the scheduler considered is waiting.":"The response carried no waiting breakdown.");
  };
  var failed=function(m){
    var limitLine=el("allocation-limit-line");if(limitLine){limitLine.textContent=m;}
    var active=el("allocation-active");if(active){active.textContent=" ";}
    var exhaustion=el("allocation-exhaustion");if(exhaustion){exhaustion.textContent=" ";}
    setList("allocation-waiting-rows",[],m);
  };
  var load=function(){
    return fetch("/v1/queue/summary").then(function(r){
      if(!r.ok){failed("Unable to read the queue summary.");return;}
      return r.json().then(render);
    }).catch(function(){failed("Unable to read the queue summary.");});
  };
  var form=el("allocation-form");
  if(form){
    form.onsubmit=function(e){
      e.preventDefault();
      var raw=el("allocation-limit").value;
      var body="{"+quote("request_id")+":"+quote(crypto.randomUUID())+
        ","+quote("scope_kind")+":"+quote(el("allocation-scope").value)+
        ","+quote("scope_value")+":"+quote(el("allocation-scope-value").value)+
        ","+quote("mode")+":"+quote(el("allocation-mode").value);
      // An empty field sends no allocation_limit key at all, so the control
      // request stores no limit and the effective limit is left alone.
      if(raw!==""){
        body+=","+quote("allocation_limit")+":{"+quote("installation_concurrent_executions")+":"+Number(raw)+"}";
      }
      body+="}";
      return fetch("/v1/controls",{method:"POST",headers:{"Content-Type":"application/json"},body:body}).then(function(r){
        return r.json().then(function(v){
          var status=el("allocation-status");
          if(status){
            status.textContent=(v&&v.revision)?("Control revision "+v.revision+" requested"+((raw!=="")?(" with a limit of "+raw+" concurrent Executions."):", with no change to the limit.")):
              ("Unable to apply: "+((v&&v.message)||"the request was refused"));
          }
          return load();
        });
      }).catch(function(){
        var status=el("allocation-status");if(status){status.textContent="Unable to apply the control request.";}
      });
    };
  }
  var button=el("allocation-refresh");
  if(button){button.onclick=load;}
  load();
  var current={requirement:"",version:0,option:""};
  var scopeRow=function(s){
    var li=document.createElement("li");line(li,"repo-state",s);return li;
  };
  var optionRow=function(o){
    var li=document.createElement("li");
    var label=document.createElement("label");
    var radio=document.createElement("input");
    radio.type="radio";radio.name="needs-input-option";radio.value=o.option_id||"";
    radio.onchange=function(){
      current.option=o.option_id||"";
      var submit=el("needs-input-submit");if(submit){submit.disabled=!current.option;}
      var state=el("needs-input-answer-state");
      if(state){state.textContent=current.option?("Selected "+current.option+". Nothing is submitted until the button is pressed."):"Nothing is submitted until an option is selected and this button is pressed.";}
    };
    label.appendChild(radio);
    var name=document.createElement("span");name.textContent=" "+(o.option_id||"unnamed option")+" \u2014 "+(o.summary||"no summary was reported");
    label.appendChild(name);
    li.appendChild(label);
    line(li,"repo-reason","impact: "+(o.impact||"no impact was reported"));
    return li;
  };
  var clear=function(message){
    el("needs-input-question").textContent="";
    el("needs-input-reason").textContent="";
    setList("needs-input-options",[],message);
    setList("needs-input-stopped",[],message);
    setList("needs-input-continuing",[],message);
    current.option="";
    var submit=el("needs-input-submit");if(submit){submit.disabled=true;}
  };
  var render=function(v){
    current.requirement=(v&&v.requirement_id)||"";
    current.version=(v&&v.version)||0;
    var q=v&&v.needs_input;
    if(!q){
      el("needs-input-state").textContent=(v&&v.status?("This Requirement is "+v.status+" and no question is recorded for it."):"The response carried no Requirement.")+" A recorded question is the only source for this section: nothing here is inferred from the status.";
      clear("No question is recorded for this Requirement.");
      return;
    }
    el("needs-input-state").textContent="Requirement "+current.requirement+" is "+((v&&v.status)||"in an unreported status")+" (v"+current.version+"), asked at "+(q.asked_at||"an unreported instant")+(q.answered_option_id?(" \u2014 already answered with "+q.answered_option_id):" \u2014 waiting for an answer");
    el("needs-input-question").textContent=q.question||"no question text was reported";
    el("needs-input-reason").textContent="why the Loop could not decide ["+(q.reason_class||"unreported class")+"]: "+(q.reason||"no reason was reported");
    setList("needs-input-options",(q.options||[]).map(optionRow),"The recorded question carried no option.");
    setList("needs-input-stopped",(q.stopped_scope||[]).map(scopeRow),"The recorded question named nothing as stopped.");
    setList("needs-input-continuing",(q.continuing_scope||[]).map(scopeRow),"The recorded question named nothing as continuing.");
  };
  var failed=function(m){
    el("needs-input-state").textContent=m;
    clear(m);
  };
  var read=function(){
    var input=el("needs-input-requirement");
    var wanted=input?input.value:"";
    if(!wanted){failed("Name a Requirement id first.");return;}
    return fetch("/v1/requirements/"+encodeURIComponent(wanted)).then(function(r){
      if(r.status===404){failed("No Requirement with that id was found.");return;}
      if(!r.ok){failed("Unable to read the Requirement detail.");return;}
      return r.json().then(render);
    }).catch(function(){failed("Unable to read the Requirement detail.");});
  };
  var answer=function(){
    if(!current.requirement||!current.option){
      el("needs-input-answer-state").textContent="Select one option first.";
      return;
    }
    return fetch("/v1/requirements/"+encodeURIComponent(current.requirement)+":answer-input",{method:"POST",...json({request_id:id(),expected_requirement_version:current.version,option_id:current.option})}).then(function(r){
      return r.json().then(function(b){
        if(!r.ok){el("needs-input-answer-state").textContent="The answer was refused: "+(b.message||"no reason was reported")+". The Requirement was not changed.";return;}
        el("needs-input-answer-state").textContent="Answered with "+(b.answered_option_id||current.option)+"; the same Requirement resumed as "+(b.status||"an unreported status")+" (v"+(b.version||"unreported")+").";
        read();
      });
    }).catch(function(){el("needs-input-answer-state").textContent="Unable to submit the answer.";});
  };
  var form=el("needs-input");
  if(form){form.onsubmit=function(e){e.preventDefault();read();};}
  var submit=el("needs-input-submit");
  if(submit){submit.onclick=answer;}
})();

// V2-074 provider compatibility and handoff. Additive, self-contained block
// appended at the end of the file; nothing above is rewritten. It renders named
// rows, never raw JSON, adds no timer and references no external asset, script
// or font. It reads only the existing owner registry read.
(function(){
  var el=function(id){return document.getElementById(id);};
  var words={
    "compatible":"inside the declared range",
    "incompatible":"outside the declared range",
    "unknown":"unknown, because an input was absent"
  };
  var reasons={
    "source-is-probing":"this Loop is spending exactly one invocation to find out whether to resume sending, so nothing is moved until that answer arrives",
    "chain-bound-reached":"this Increment has already used its stated number of moves, so it is not moved again",
    "candidate-needs-an-owner-action":"every other Provider needs an action only the owner can take, such as signing in to a CLI or clearing a stop",
    "candidate-is-measured-incompatible":"every other Provider was measured outside a declared version range",
    "candidate-already-tried-for-this-increment":"every other Provider has already been tried for this Increment, and it is not handed back",
    "candidate-is-not-sendable":"no other Provider is one this Loop is sending to right now"
  };
  var dispositions={
    "none":"nothing needs to move",
    "handoff-proposed":"a destination is proposed",
    "waiting":"waiting"
  };
  var range=function(interval){
    if(!interval){return "no range was reported";}
    return "from "+(interval.from||"an unreported bound")+" up to but not including "+(interval.until||"an unreported bound");
  };
  var setList=function(id,items,empty){
    var list=el(id);
    if(!list){return;}
    while(list.firstChild){list.removeChild(list.firstChild);}
    if(!items.length){
      var none=document.createElement("li");
      none.className="muted";
      none.textContent=empty;
      list.appendChild(none);
      return;
    }
    for(var i=0;i<items.length;i++){
      var row=document.createElement("li");
      row.textContent=items[i];
      list.appendChild(row);
    }
  };
  var describe=function(entry){
    var compat=entry.compatibility||{};
    var handoff=entry.handoff||{};
    var loopVersion=compat.observed_loop_version?("this Loop reports version "+compat.observed_loop_version):"this Loop reports no version, because it was given no explicit release source root";
    var where=dispositions[handoff.disposition]||"an unreported disposition";
    if(handoff.disposition==="handoff-proposed"){
      where="a destination is proposed: "+(handoff.target||"an unreported Provider");
    }
    if(handoff.disposition==="waiting"){
      where="waiting \u2014 "+(reasons[handoff.waiting_reason]||("reason "+(handoff.waiting_reason||"unreported")));
    }
    return entry.provider
      +" \u2014 supported CLI versions "+range(compat.cli_version_interval)
      +"; CLI state "+(words[compat.cli_compatibility]||"unreported")
      +". Supported Loop versions "+range(compat.loop_version_interval)
      +"; "+loopVersion
      +"; Loop state "+(words[compat.loop_compatibility]||"unreported")
      +". Allocation is "+((entry.concurrency&&entry.concurrency.exhausted)?"exhausted":"not exhausted")
      +". Now: "+where+".";
  };
  var render=function(v){
    var rows=(v&&v.providers)||[];
    if(!rows.length){
      setList("provider-handoff-rows",[],"The response carried no Provider.");
      setList("provider-handoff-waiting",[],"The response carried no Provider.");
      setList("provider-handoff-proposed",[],"The response carried no Provider.");
      el("provider-handoff-state").textContent="The response carried no Provider row.";
      return;
    }
    var all=[];
    var waiting=[];
    var proposed=[];
    var unknowns=0;
    for(var i=0;i<rows.length;i++){
      var entry=rows[i];
      all.push(describe(entry));
      var handoff=entry.handoff||{};
      var compat=entry.compatibility||{};
      if(compat.cli_compatibility==="unknown"||compat.loop_compatibility==="unknown"){unknowns++;}
      if(handoff.disposition==="waiting"){
        waiting.push(entry.provider+" \u2014 "+(reasons[handoff.waiting_reason]||("reason "+(handoff.waiting_reason||"unreported"))));
      }
      if(handoff.disposition==="handoff-proposed"){
        proposed.push(entry.provider+" \u2014 work would go to "+(handoff.target||"an unreported Provider")+". This is a proposal only; nothing here carries it out.");
      }
    }
    setList("provider-handoff-rows",all,"The response carried no Provider.");
    setList("provider-handoff-waiting",waiting,"No Provider is waiting.");
    setList("provider-handoff-proposed",proposed,"No destination is proposed.");
    el("provider-handoff-state").textContent="Read "+rows.length+" Providers; "+unknowns+" of them report at least one state as unknown, which means an input was absent rather than that everything is fine. Every range shown is a declaration this repository owns, and no part of this page establishes that a range is true of a real CLI.";
  };
  var failed=function(m){
    el("provider-handoff-state").textContent=m;
    setList("provider-handoff-rows",[],m);
    setList("provider-handoff-waiting",[],m);
    setList("provider-handoff-proposed",[],m);
  };
  var read=function(){
    return fetch("/v1/providers").then(function(r){
      if(!r.ok){failed("Unable to read the Provider registry.");return;}
      return r.json().then(render);
    }).catch(function(){failed("Unable to read the Provider registry.");});
  };
  var refresh=el("provider-handoff-refresh");
  if(refresh){refresh.onclick=read;}
})();
