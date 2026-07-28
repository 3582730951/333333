#!/usr/bin/env python3
"""Async streaming curl_cffi sidecar. Wire-compatible with the legacy /proxy API."""
import asyncio, base64, hashlib, inspect, json, os, resource, shutil, signal, sys, tempfile, time
from collections import OrderedDict
from typing import Any
from curl_cffi.const import CurlOpt
from curl_cffi.requests import AsyncSession

COOKIE_DIR=os.getenv("CODEX_POOL_SIDECAR_COOKIE_DIR",os.path.join(tempfile.gettempdir(),"codex-pool-sidecar-cookies"))
IMPERSONATE=os.getenv("CODEX_POOL_SIDECAR_IMPERSONATE","chrome120")
TIMEOUT=int(os.getenv("CODEX_POOL_SIDECAR_TIMEOUT","120")); DRAIN_SECONDS=int(os.getenv("CODEX_POOL_SIDECAR_DRAIN_SECONDS","20"))
MAX_BUCKETS=int(os.getenv("CODEX_POOL_SIDECAR_POOL_MAX_KEYS","4096")); SESSION_TTL=int(os.getenv("CODEX_POOL_SIDECAR_SESSION_TTL","900"))
MAX_CLIENTS=int(os.getenv("CODEX_POOL_SIDECAR_MAX_CLIENTS","512"))
ACCEPT_ENCODING=os.getenv("CODEX_POOL_SIDECAR_ACCEPT_ENCODING","gzip, deflate")
COOKIE_FLUSH=float(os.getenv("CODEX_POOL_SIDECAR_COOKIE_FLUSH_SECONDS","0.25"))
SPOOL_DIR=os.getenv("CODEX_POOL_SIDECAR_SPOOL_DIR",tempfile.gettempdir())
MAX_BODY_BYTES=int(os.getenv("CODEX_POOL_SIDECAR_MAX_BODY_BYTES",str(1<<30)))
LEGACY_BODY_MAX_BYTES=int(os.getenv("CODEX_POOL_SIDECAR_LEGACY_BODY_MAX_BYTES",str(64<<20)))
SPOOL_MAX_BYTES=int(os.getenv("CODEX_POOL_SIDECAR_SPOOL_MAX_BYTES",str(32<<30)))
SPOOL_RESERVE_BYTES=int(os.getenv("CODEX_POOL_SIDECAR_SPOOL_RESERVE_BYTES",str(10<<30)))
_sessions:OrderedDict[tuple,tuple[AsyncSession,float]]=OrderedDict();_session_active:dict[tuple,int]={};_cookies:dict[str,dict[str,str]]={};_dirty:set[str]=set();_inflight=0;_handles=0;_spool_bytes=0;_stopping=False

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

class SidecarBodyError(Exception):
 def __init__(self,code:str,status:int,retryable:bool,message:str):super().__init__(message);self.code=code;self.status=status;self.retryable=retryable

class SpoolBody:
 def __init__(self,file,size:int):self.file=file;self.path=file.name;self.size=size;self.closed=False
 def rewind(self):self.file.flush();self.file.seek(0)
 def close(self):
  global _spool_bytes
  if self.closed:return
  self.closed=True
  try:self.file.close()
  finally:
   try:os.remove(self.path)
   except FileNotFoundError:pass
   _spool_bytes=max(0,_spool_bytes-self.size)

async def spool_request_body(reader:asyncio.StreamReader,size:int)->SpoolBody:
 global _spool_bytes
 if size>MAX_BODY_BYTES:raise SidecarBodyError("sidecar_body_too_large",413,False,f"request body exceeds {MAX_BODY_BYTES} bytes")
 if size>SPOOL_MAX_BYTES-_spool_bytes:raise SidecarBodyError("sidecar_spool_exhausted",503,True,"sidecar spool budget exhausted")
 os.makedirs(SPOOL_DIR,mode=0o700,exist_ok=True)
 if shutil.disk_usage(SPOOL_DIR).free-size<SPOOL_RESERVE_BYTES:raise SidecarBodyError("sidecar_disk_reserve",503,True,"sidecar spool disk reserve reached")
 file=tempfile.NamedTemporaryFile(mode="w+b",prefix="codex-pool-sidecar-body-",dir=SPOOL_DIR,delete=False)
 _spool_bytes+=size
 body=SpoolBody(file,size)
 try:
  remaining=size
  while remaining:
   chunk=await reader.readexactly(min(64<<10,remaining));file.write(chunk);remaining-=len(chunk)
  body.rewind();return body
 except Exception:
  body.close();raise

async def read_request(reader:asyncio.StreamReader):
 head=await reader.readuntil(b"\r\n\r\n");lines=head.decode("latin1").split("\r\n");method,path,_=lines[0].split(" ",2);headers={}
 for line in lines[1:]:
  if ":" in line:k,v=line.split(":",1);headers[k.lower()]=v.strip()
 try:n=int(headers.get("content-length","0"))
 except ValueError:raise SidecarBodyError("sidecar_invalid_content_length",400,False,"invalid content-length")
 if n<0:raise SidecarBodyError("sidecar_invalid_content_length",400,False,"negative content-length")
 if path=="/proxy" and headers.get("x-sidecar-meta") and n:body=await spool_request_body(reader,n)
 else:
  if n>LEGACY_BODY_MAX_BYTES:raise SidecarBodyError("sidecar_legacy_body_too_large",413,False,f"legacy sidecar body exceeds {LEGACY_BODY_MAX_BYTES} bytes")
  body=await reader.readexactly(n) if n else b""
 return method,path,headers,body
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
 return {"ok":True,"inflight":_inflight,"curl_handles":_handles,"session_buckets":len(_sessions),"rss_bytes":rss*(1024 if sys.platform!="darwin" else 1),"spool_bytes":_spool_bytes,"spool_limit_bytes":SPOOL_MAX_BYTES,"impersonate":IMPERSONATE,"draining":_stopping}

# A transport error after curl has begun a request is delivery-ambiguous: the
# upstream may already have accepted a stateful Responses turn. Only the narrow
# preflight class below is safe for the Go relay to retry or bypass.
class SidecarPreflightError(Exception): pass
class SidecarDeliveryUnknownError(Exception): pass

async def proxy(writer,payload:dict[str,Any],body:Any):
 global _handles
 upload_session=None;key=None
 try:
  method=str(payload.get("method") or "POST").upper();url=str(payload["url"]);jar=str(payload.get("cookie_jar_key") or "default");proxy_url=str(payload.get("proxy") or "").strip();ja3=str(payload.get("ja3") or "").strip();akamai=str(payload.get("akamai") or "").strip()
  if isinstance(body,SpoolBody):
   body.rewind();options={CurlOpt.POST:0,CurlOpt.UPLOAD:1,CurlOpt.CUSTOMREQUEST:method.encode(),CurlOpt.INFILESIZE_LARGE:body.size,CurlOpt.READDATA:body.file,CurlOpt.UPLOAD_BUFFERSIZE:64<<10}
   upload_session=AsyncSession(impersonate=IMPERSONATE,max_clients=1,curl_options=options);session=upload_session;request_data=None
  else:
   key=(proxy_url,ja3,akamai,jar);session=await session_for(key);request_data=body or None
  kwargs={"headers":clean_headers(payload.get("headers") or {}),"data":request_data,"cookies":load_cookies(jar),"timeout":TIMEOUT,"accept_encoding":ACCEPT_ENCODING}
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
  if upload_session is not None:await upload_session.close()
  raise SidecarPreflightError(str(e)) from e
 if key is not None:_session_active[key]=_session_active.get(key,0)+1
 _handles+=1;started=False
 try:
  async with session.stream(method,url,**kwargs) as resp:
   merged=load_cookies(jar)
   try:merged.update(session.cookies.get_dict());save_cookies(jar,merged)
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
 finally:
  _handles-=1
  if key is not None:_session_active[key]=max(0,_session_active.get(key,1)-1)
  if upload_session is not None:await upload_session.close()

async def client(reader,writer):
 global _inflight
 raw=None
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
 except SidecarBodyError as e:
  if not writer.is_closing():await send(writer,e.status,{"content-type":"application/json","retry-after":"1" if e.retryable else "0","x-sidecar-error-code":e.code,"x-sidecar-error-phase":"preflight","x-sidecar-error-retryable":"true" if e.retryable else "false"},structured_error(e.code,"preflight",e.retryable,str(e)))
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
  if isinstance(raw,SpoolBody):raw.close()
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
