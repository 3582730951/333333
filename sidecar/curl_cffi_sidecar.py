#!/usr/bin/env python3
"""Async streaming curl_cffi sidecar. Wire-compatible with the legacy /proxy API."""
import asyncio, base64, hashlib, inspect, json, os, resource, signal, sys, tempfile, time
from collections import OrderedDict
from typing import Any
from curl_cffi.requests import AsyncSession

COOKIE_DIR=os.getenv("CODEX_POOL_SIDECAR_COOKIE_DIR",os.path.join(tempfile.gettempdir(),"codex-pool-sidecar-cookies"))
IMPERSONATE=os.getenv("CODEX_POOL_SIDECAR_IMPERSONATE","chrome120")
TIMEOUT=int(os.getenv("CODEX_POOL_SIDECAR_TIMEOUT","120")); DRAIN_SECONDS=int(os.getenv("CODEX_POOL_SIDECAR_DRAIN_SECONDS","20"))
MAX_BUCKETS=int(os.getenv("CODEX_POOL_SIDECAR_POOL_MAX_KEYS","4096")); SESSION_TTL=int(os.getenv("CODEX_POOL_SIDECAR_SESSION_TTL","900"))
MAX_CLIENTS=int(os.getenv("CODEX_POOL_SIDECAR_MAX_CLIENTS","512"))
ACCEPT_ENCODING=os.getenv("CODEX_POOL_SIDECAR_ACCEPT_ENCODING","gzip, deflate")
COOKIE_FLUSH=float(os.getenv("CODEX_POOL_SIDECAR_COOKIE_FLUSH_SECONDS","0.25"))
_sessions:OrderedDict[tuple,tuple[AsyncSession,float]]=OrderedDict();_session_active:dict[tuple,int]={};_cookies:dict[str,dict[str,str]]={};_dirty:set[str]=set();_inflight=0;_handles=0;_stopping=False

def safe_key(v:str)->str:return hashlib.sha256(v.encode()).hexdigest()
def cookie_path(k:str)->str:return os.path.join(COOKIE_DIR,safe_key(k)+".json")
def load_cookies(k:str)->dict[str,str]:
 if k in _cookies:return dict(_cookies[k])
 try:
  with open(cookie_path(k),encoding="utf-8") as f:d=json.load(f)
  d={str(x):str(y) for x,y in d.items()} if isinstance(d,dict) else {}
 except Exception:d={}
 _cookies[k]=d;return dict(d)
def save_cookies(k:str,d:dict[str,str])->None:_cookies[k]=dict(d);_dirty.add(k)
async def cookie_writer():
 while not _stopping or _dirty:
  await asyncio.sleep(COOKIE_FLUSH);os.makedirs(COOKIE_DIR,exist_ok=True)
  for k in list(_dirty):
   try:
    tmp=cookie_path(k)+".tmp";open(tmp,"w",encoding="utf-8").write(json.dumps(_cookies.get(k,{}),sort_keys=True));os.replace(tmp,cookie_path(k));_dirty.discard(k)
   except Exception:pass

def clean_headers(h:dict[str,Any])->dict[str,str]:
 return {k:(str(v[-1]) if isinstance(v,list) and v else str(v)) for k,v in h.items() if k.lower() not in {"host","content-length","connection","accept-encoding"}}
async def session_for(key:tuple)->AsyncSession:
 now=time.monotonic()
 for k,(s,t) in list(_sessions.items()):
  if now-t>SESSION_TTL and not _session_active.get(k,0):_sessions.pop(k,None);_session_active.pop(k,None);await s.close()
 if key in _sessions:
  s,_=_sessions.pop(key);_sessions[key]=(s,now);return s
 s=AsyncSession(impersonate=IMPERSONATE,max_clients=MAX_CLIENTS);_sessions[key]=(s,now)
 while len(_sessions)>MAX_BUCKETS:
  victim=next((k for k in _sessions if not _session_active.get(k,0)),None)
  if victim is None:break
  old,_=_sessions.pop(victim);_session_active.pop(victim,None);await old.close()
 return s

async def read_request(reader:asyncio.StreamReader):
 head=await reader.readuntil(b"\r\n\r\n");lines=head.decode("latin1").split("\r\n");method,path,_=lines[0].split(" ",2);headers={}
 for line in lines[1:]:
  if ":" in line:k,v=line.split(":",1);headers[k.lower()]=v.strip()
 n=int(headers.get("content-length","0"));body=await reader.readexactly(n) if n else b"";return method,path,headers,body
async def send(writer,status:int,headers:dict[str,str],body:bytes=b""):
 reason={200:"OK",400:"Bad Request",404:"Not Found",502:"Bad Gateway",503:"Service Unavailable"}.get(status,"OK");headers=dict(headers);headers.setdefault("content-length",str(len(body)));headers["connection"]="close"
 writer.write((f"HTTP/1.1 {status} {reason}\r\n"+"".join(f"{k}: {v}\r\n" for k,v in headers.items())+"\r\n").encode("latin1")+body);await writer.drain()
async def send_chunk(writer,body:bytes):
 writer.write(f"{len(body):X}\r\n".encode("ascii")+body+b"\r\n");await writer.drain()
async def finish_chunked(writer,code:str="",phase:str="",retryable:bool=False):
 trailers=""
 if code:
  trailers+=f"X-Sidecar-Stream-Error-Code: {code}\r\nX-Sidecar-Stream-Error-Phase: {phase or 'stream'}\r\nX-Sidecar-Stream-Error-Retryable: {'true' if retryable else 'false'}\r\n"
 writer.write(("0\r\n"+trailers+"\r\n").encode("latin1"));await writer.drain()
def structured_error(code:str,phase:str,retryable:bool,message:str)->bytes:
 return json.dumps({"error":{"code":code,"phase":phase,"retryable":retryable,"message":message}},separators=(",",":"),ensure_ascii=False).encode()
def metrics():
 rss=resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
 return {"ok":True,"inflight":_inflight,"curl_handles":_handles,"session_buckets":len(_sessions),"rss_bytes":rss*(1024 if sys.platform!="darwin" else 1),"impersonate":IMPERSONATE,"draining":_stopping}

# A transport error after curl has begun a request is delivery-ambiguous: the
# upstream may already have accepted a stateful Responses turn. Only the narrow
# preflight class below is safe for the Go relay to retry or bypass.
class SidecarPreflightError(Exception): pass
class SidecarDeliveryUnknownError(Exception): pass

async def proxy(writer,payload:dict[str,Any],body:bytes):
 global _handles
 try:
  method=str(payload.get("method") or "POST").upper();url=str(payload["url"]);jar=str(payload.get("cookie_jar_key") or "default");proxy_url=str(payload.get("proxy") or "").strip();ja3=str(payload.get("ja3") or "").strip();akamai=str(payload.get("akamai") or "").strip()
  key=(proxy_url,ja3,akamai,jar);session=await session_for(key);kwargs={"headers":clean_headers(payload.get("headers") or {}),"data":body or None,"cookies":load_cookies(jar),"timeout":TIMEOUT,"accept_encoding":ACCEPT_ENCODING}
  if proxy_url:kwargs["proxy"]=proxy_url
  # default_headers gates curl-impersonate's INJECTION of the browser's own header set
  # (sec-ch-ua*, sec-fetch-*, accept-language, upgrade-insecure-requests, …) ON TOP of the
  # caller-supplied headers. An explicit ja3 already disables it (exact-header replay). When
  # no ja3 is set the caller may still opt out via {"default_headers": false}: the Go layer
  # builds a complete, authentic client header set from scratch (e.g. the claude-cli/Node
  # fingerprint), so the browser extras are pure noise that would out the request as an
  # impersonation relay — while the TLS/HTTP2 impersonation (session impersonate=) is kept.
  # Absent field => True (curl default), so existing Codex/registration callers are unchanged.
  if ja3:kwargs.update(ja3=ja3,default_headers=False)
  elif payload.get("default_headers") is False:kwargs["default_headers"]=False
  if akamai:kwargs["akamai"]=akamai
  if payload.get("allow_redirects") is False:kwargs["allow_redirects"]=False
 except Exception as e:
  raise SidecarPreflightError(str(e)) from e
 _session_active[key]=_session_active.get(key,0)+1
 _handles+=1;started=False
 try:
  async with session.stream(method,url,**kwargs) as resp:
   merged=load_cookies(jar)
   try:merged.update(resp.cookies.get_dict());save_cookies(jar,merged)
   except Exception:pass
   reported={k:[v] for k,v in resp.headers.items() if k.lower() not in {"content-encoding","content-length","transfer-encoding"}}
   # v2 never emits another HTTP status once this header block is written. Any
   # later curl/read failure is represented through chunked trailers, which Go can
   # observe after consuming the stream and turn into a legal downstream terminal.
   enc=base64.b64encode(json.dumps(reported).encode()).decode();writer.write(("HTTP/1.1 200 OK\r\n"+f"x-sidecar-upstream-status: {resp.status_code}\r\nx-sidecar-upstream-headers-b64: {enc}\r\ncontent-type: {resp.headers.get('content-type','application/octet-stream')}\r\ntransfer-encoding: chunked\r\ntrailer: X-Sidecar-Stream-Error-Code, X-Sidecar-Stream-Error-Phase, X-Sidecar-Stream-Error-Retryable\r\nconnection: close\r\n\r\n").encode("latin1"));await writer.drain();started=True
   async for chunk in resp.aiter_content():
    if chunk:await send_chunk(writer,chunk)
   await finish_chunked(writer)
 except Exception as e:
  if started:
   # Writing the trailer may itself fail after the client disconnects. Either way
   # do not re-raise into client(), which would otherwise append a second 502 line.
   try:await finish_chunked(writer,"sidecar_stream_error","stream",True)
   except Exception:pass
   return
  raise SidecarDeliveryUnknownError(str(e)) from e
 finally:_handles-=1;_session_active[key]=max(0,_session_active.get(key,1)-1)

async def client(reader,writer):
 global _inflight
 try:
  method,path,h,raw=await read_request(reader)
  if method=="GET" and path in {"/healthz","/metrics"}:await send(writer,200,{"content-type":"application/json"},json.dumps(metrics()).encode());return
  if method!="POST" or path not in {"/proxy","/cookies"}:await send(writer,404,{"content-type":"application/json"},b'{"error":"not found"}');return
  if _stopping:await send(writer,503,{"content-type":"application/json","retry-after":"1"},b'{"error":"draining"}');return
  if path=="/cookies":
   p=json.loads(raw);k=str(p.get("cookie_jar_key") or "default");d=load_cookies(k);d.update({str(x):str(y) for x,y in (p.get("cookies") or {}).items()});save_cookies(k,d);await send(writer,200,{"content-type":"application/json"},json.dumps({"ok":True,"count":len(d)}).encode());return
  _inflight+=1
  if h.get("x-sidecar-meta"):p=json.loads(base64.b64decode(h["x-sidecar-meta"]));body=raw
  else:p=json.loads(raw);body=base64.b64decode(str(p.get("body_b64") or ""))
  await proxy(writer,p,body)
 except SidecarPreflightError as e:
  # This is the only pre-header error certified not to have reached the upstream.
  # The v2 retryability header is an explicit contract with the Go relay.
  if not writer.is_closing():await send(writer,502,{"content-type":"application/json","x-sidecar-error-code":"sidecar_preflight_error","x-sidecar-error-phase":"preflight","x-sidecar-error-retryable":"true"},structured_error("sidecar_preflight_error","preflight",True,str(e)))
 except SidecarDeliveryUnknownError as e:
  # Do not invite a replay: curl may have sent request bytes before its failure.
  if not writer.is_closing():await send(writer,502,{"content-type":"application/json","x-sidecar-error-code":"sidecar_delivery_unknown","x-sidecar-error-phase":"request","x-sidecar-error-retryable":"false"},structured_error("sidecar_delivery_unknown","request",False,str(e)))
 except Exception as e:
  # Malformed local requests are structured for diagnosis but are never retried.
  if not writer.is_closing():await send(writer,502,{"content-type":"application/json","x-sidecar-error-code":"sidecar_request_error","x-sidecar-error-phase":"request","x-sidecar-error-retryable":"false"},structured_error("sidecar_request_error","request",False,str(e)))
 finally:
  if 'path' in locals() and path=="/proxy":_inflight=max(0,_inflight-1)
  writer.close();await writer.wait_closed()

async def main(addr:str):
 global _stopping
 host,port=addr.rsplit(":",1);loop=asyncio.get_running_loop();stop=asyncio.Event()
 for sig in (signal.SIGTERM,signal.SIGINT):loop.add_signal_handler(sig,stop.set)
 server=await asyncio.start_server(client,host,int(port),backlog=1024);writer=asyncio.create_task(cookie_writer());print(f"curl_cffi async sidecar listening on {addr}",flush=True)
 async with server:await stop.wait();_stopping=True;server.close();await server.wait_closed()
 deadline=time.monotonic()+DRAIN_SECONDS
 while _inflight and time.monotonic()<deadline:await asyncio.sleep(.05)
 await writer
 for s,_ in list(_sessions.values()):await s.close()

def selftest():
 assert safe_key("x")==hashlib.sha256(b"x").hexdigest();assert inspect.iscoroutinefunction(proxy);print("All tests passed")
if __name__=="__main__":
 if "--selftest" in sys.argv:selftest()
 else:asyncio.run(main(os.getenv("CODEX_POOL_SIDECAR_ADDR","127.0.0.1:8790")))
