(function(){
'use strict';
const data=JSON.parse(document.getElementById('gpu-data').textContent);
const devices=data.devices||[], summaryMount=document.getElementById('gpu-summary-mount');
const comparison=document.getElementById('gpu-comparison'), profile=document.getElementById('profile-view');
const flameTab=document.getElementById('gpu-tab-flame'), compareTab=document.getElementById('gpu-tab-compare');
let selected=0,collapsed=false;
const esc=v=>String(v==null?'':v).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const date=v=>v?new Date(v).toISOString().replace('T',' ').replace('Z',' UTC'):'—';
const duration=((new Date(data.ended_at)-new Date(data.started_at))/1000).toFixed(3);
const defs=[
 ['gpu.temperature_celsius','Temperature','°C',1,1],
 ['gpu.power_watts','Power','W',1,1],
 ['gpu.memory.used_bytes','Memory used','GiB',1/2**30,2],
 ['gpu.utilization_ratio','GPU utilization','%',100,1],
 ['gpu.sm_clock_hertz','SM clock','MHz',1e-6,0],
 ['gpu.memory_activity_ratio','Memory activity','%',100,1],
 ['gpu.power_limit_watts','Configured power limit','W',1,1],
 ['gpu.memory.total_bytes','Memory total','GiB',1/2**30,2],
 ['gpu.memory.free_bytes','Memory free','GiB',1/2**30,2],
 ['gpu.memory.reserved_bytes','Memory reserved','GiB',1/2**30,2],
 ['gpu.memory_clock_hertz','Memory clock','MHz',1e-6,0]
];
const reasons=[['idle','Idle'],['application_clocks','Application clocks'],['power_cap','Power cap'],['hardware_slowdown','Hardware slowdown'],['sync_boost','Sync boost'],['software_thermal','Software thermal limit'],['hardware_thermal','Hardware thermal limit'],['hardware_power_brake','Hardware power brake'],['display_clock','Display clock'],['board_limit','Board limit'],['reliability','Reliability']];
const scope='Whole physical GPU, including other processes. Frame zoom does not change these aggregates.';
function metric(d,key){return d.metrics&&d.metrics[key];}
function value(s,field,def){return !s||!s.count?'—':(s[field]*def[3]).toLocaleString('en-US',{minimumFractionDigits:def[4],maximumFractionDigits:def[4]});}
function absence(d,key){return d.unsupported&&d.unsupported[key]?'Unsupported: '+d.unsupported[key]:d.unavailable&&d.unavailable[key]?'Unavailable: '+d.unavailable[key]:'No valid readings';}
function tag(d){const failed=d&&Object.keys(d.unavailable||{}).length;return '<span class="gpu-tag'+(failed?' warning':'')+'">'+(d?d.readings+' readings'+(failed?' · read errors':''):'No observations')+'</span>';}
function windowLine(){return '<div class="gpu-window"><span><b>'+esc(date(data.started_at))+' → '+esc(date(data.ended_at))+'</b></span><span>'+duration+' s wall-clock</span><span>Capture <code>'+esc(data.capture_id)+'</code></span><span class="scope">Full capture lifecycle · includes setup</span></div>';}
function kpis(d){return '<div class="gpu-kpis">'+defs.slice(0,6).map(def=>{
 const s=metric(d,def[0]);let foot=s&&s.count?'max <b>'+value(s,'max',def)+' '+def[2]+'</b>':esc(absence(d,def[0]));
 if(def[0]==='gpu.memory.used_bytes'){const total=metric(d,'gpu.memory.total_bytes');if(total&&total.count)foot='of '+value(total,'mean',defs[7])+' GiB total (mean)<br>'+foot;}
 if(s&&s.count&&s.count<d.readings)foot+='<br>'+s.count+' valid / '+d.readings+' readings';
 const tip=def[0]==='gpu.utilization_ratio'?'Time with at least one kernel running during NVML sampling periods; not SM occupancy or throughput':def[0]==='gpu.memory_activity_ratio'?'Time with global memory reads/writes during NVML sampling periods; not memory capacity used':'';
 return '<div class="gpu-kpi" data-metric="'+def[0]+'"><span class="gpu-kpi-label" title="'+tip+'">'+def[1]+' · mean</span><div class="gpu-number">'+value(s,'mean',def)+'<small>'+def[2]+'</small></div><div class="gpu-kpi-foot">'+foot+'</div></div>';
 }).join('')+'</div>';}
function details(d){
 const table='<div class="gpu-table-wrap"><table class="gpu-table"><thead><tr><th>Metric</th><th>Min</th><th>Mean</th><th>Max</th><th>Last valid</th><th>Valid</th></tr></thead><tbody>'+defs.map(def=>{
  const key=def[0],s=metric(d,key);let row='<tr><td>'+def[1]+' ('+def[2]+')</td>'+['min','mean','max','last'].map(f=>'<td>'+value(s,f,def)+'</td>').join('')+'<td>'+(s?s.count:0)+'</td></tr>';
  const notes=[];if(!s||!s.count)notes.push(absence(d,key));if(d.failed_readings&&d.failed_readings[key])notes.push(d.failed_readings[key]+' read errors: '+d.unavailable[key]);if(s&&s.count&&d.unsupported&&d.unsupported[key])notes.push('Unsupported readings: '+d.unsupported[key]);if(s&&s.count)notes.push('Last valid: '+date(s.last_at));
  return row+'<tr class="reason"><td colspan="6">'+esc(notes.join(' · '))+'</td></tr>';
 }).join('')+'</tbody></table></div>';
 const events=reasons.map(([key,label])=>{const name='gpu.clock_event_active.'+key,s=metric(d,name);return '<p>'+label+': <b>'+(s&&s.count?Math.round(s.mean*s.count)+' / '+s.count+' valid readings':esc(absence(d,name)))+'</b></p>';}).join('');
 return table+'<div class="gpu-quality"><div><h3>Observations</h3><p><b>'+d.readings+'</b> device readings</p><p>First: '+esc(date(d.first_at))+'</p><p>Last: '+esc(date(d.last_at))+'</p><p>Configured interval: '+(data.interval_ns/1e9).toLocaleString('en-US')+' s</p><p>Readings may cover only part of the window. Counts do not imply uninterrupted sampling.</p><p>Driver: '+esc(d.driver_version||'unknown')+'</p><p>Memory API: '+esc(d.memory_api_version||'unknown')+'</p></div><div><h3>Clock event observations</h3>'+events+'<p>Active observations, not event counts or durations. Idle is separate from thermal limits.</p></div></div><p class="gpu-method">Mean = arithmetic mean of valid observations. Missing readings are excluded; no interpolation. Each metric has its own count and last valid time. GPU utilization measures time with at least one kernel running, not SM occupancy or throughput. Memory activity measures reads/writes, not capacity. Raw NVML memory accounting is retained; GiB = 2³⁰ bytes. The capture lifecycle includes attachment/setup; CPU/off-CPU collection may still drain briefly after the boundary. Profile totals accumulate sample values and are not wall-clock duration.</p>';
}
function empty(){return '<div class="gpu-empty"><h3>No observations in this capture window</h3><p>No device values can be reported for this profile period.</p>'+(data.errors||[]).map(e=>'<p>'+esc(e)+'</p>').join('')+'</div>';}
function card(d,index,compact){const id=(compact?'summary':'compare')+'-details-'+index;
 const title=compact?'GPU telemetry':esc(d.name||'Physical GPU');
 const heading='<div class="gpu-heading"><h2>'+title+'</h2><span class="gpu-sub">'+(compact?'Selected: '+esc(d?d.uuid:'none'):'Physical GPU')+'</span><span class="gpu-grow"></span>'+tag(d)+(compact?'<div class="gpu-actions"><button type="button" class="compare-action">Compare GPUs ↗</button><button type="button" id="gpu-collapse" aria-controls="gpu-summary-content" aria-expanded="'+!collapsed+'">'+(collapsed?'Show summary ⌄':'Hide summary ⌃')+'</button></div>':'')+'</div>';
 const selector=compact&&d?'<div class="gpu-device-line"><label>Device <select id="gpu-select" aria-label="Select physical GPU">'+devices.map((v,i)=>'<option value="'+i+'" '+(i===selected?'selected':'')+'>'+esc(v.name||v.uuid)+' · '+esc(v.uuid)+'</option>').join('')+'</select></label></div>':d?'<div class="gpu-device-line"><code class="gpu-uuid">'+esc(d.uuid)+'</code></div>':'';
 const footer=d?'<div class="gpu-footer"><span>Mean of valid samples · full window</span><button type="button" class="gpu-detail-toggle" aria-controls="'+id+'" aria-expanded="false">Details & observations ↗</button>'+(!compact?'<button type="button" class="choose-summary" data-index="'+index+'">Use in summary</button>':'')+'</div><div id="'+id+'" class="gpu-details" hidden>'+details(d)+'</div>':'';
 return '<section class="gpu-shell'+(compact&&collapsed?' collapsed':'')+(!compact&&index===selected?' selected':'')+'"'+(compact?' id="gpu-summary"':'')+'>'+heading+(compact?'<div id="gpu-summary-content"'+(collapsed?' hidden':'')+'>'+windowLine():'')+selector+(d?kpis(d):empty())+(compact?'<div class="gpu-scope-note">'+scope+'</div>':'')+footer+(compact?'</div>':'')+'</section>';
}
function setView(compare){
 profile.hidden=compare;summaryMount.hidden=compare;comparison.hidden=!compare;
 document.body.classList.toggle('gpu-comparing',compare);
 flameTab.setAttribute('aria-selected',String(!compare));compareTab.setAttribute('aria-selected',String(compare));flameTab.tabIndex=compare?-1:0;compareTab.tabIndex=compare?0:-1;
}
function render(){
 summaryMount.innerHTML=card(devices[selected],selected,true);
 comparison.classList.add('gpu-overview');
 comparison.innerHTML='<div class="gpu-overview-head"><div><h2>GPU telemetry</h2><p>'+devices.length+' physical GPUs · capture window aggregates</p></div><span class="gpu-grow"></span><button type="button" id="gpu-back">← Back to flame graph</button></div>'+windowLine()+(devices.length?'<div class="gpu-overview-grid">'+devices.map((d,i)=>card(d,i,false)).join('')+'</div>':empty())+'<div class="gpu-page-method">'+scope+'</div>';
 if(devices.length&&(data.errors||[]).length){const notice=document.createElement('div');notice.className='gpu-message';notice.textContent=data.errors.join(' · ');summaryMount.querySelector('#gpu-summary-content').append(notice);comparison.append(notice.cloneNode(true));}
 document.getElementById('gpu-collapse').addEventListener('click',()=>{collapsed=!collapsed;document.getElementById('gpu-summary-content').hidden=collapsed;document.getElementById('gpu-summary').classList.toggle('collapsed',collapsed);const b=document.getElementById('gpu-collapse');b.textContent=collapsed?'Show summary ⌄':'Hide summary ⌃';b.setAttribute('aria-expanded',String(!collapsed));});
 summaryMount.querySelector('.compare-action').addEventListener('click',()=>{setView(true);compareTab.focus();});
 document.getElementById('gpu-back').addEventListener('click',()=>{setView(false);flameTab.focus();});
 const selector=document.getElementById('gpu-select');if(selector)selector.addEventListener('change',()=>{selected=Number(selector.value);render();document.getElementById('gpu-select').focus();});
 document.querySelectorAll('.gpu-detail-toggle').forEach(b=>b.addEventListener('click',()=>{const panel=document.getElementById(b.getAttribute('aria-controls'));panel.hidden=!panel.hidden;b.setAttribute('aria-expanded',String(!panel.hidden));b.textContent=panel.hidden?'Details & observations ↗':'Hide details ↙';}));
 comparison.querySelectorAll('.choose-summary').forEach(b=>b.addEventListener('click',()=>{selected=Number(b.dataset.index);collapsed=false;render();setView(false);document.getElementById('gpu-select').focus();}));
}
flameTab.addEventListener('click',()=>setView(false));compareTab.addEventListener('click',()=>setView(true));
document.querySelector('.gpu-tabs').addEventListener('keydown',e=>{if(['ArrowLeft','ArrowRight','Home','End'].includes(e.key)){e.preventDefault();const compare=e.key==='End'||(e.key!=='Home'&&comparison.hidden);setView(compare);(compare?compareTab:flameTab).focus();}});
render();setView(false);
})();
