package main

import (
 "encoding/json"
 "io"
 "os"
 "sort"
 "github.com/hollis-labs/tether/internal/shimcodex"
)

func keys(m map[string]json.RawMessage) []string { out:=make([]string,0,len(m)); for k:=range m {out=append(out,k)}; sort.Strings(out); return out }
func main() {
 raw,err:=io.ReadAll(io.LimitReader(os.Stdin,136<<20)); if err!=nil {panic("bounded input unavailable")}
 var s shimcodex.State; if json.Unmarshal(raw,&s)!=nil {panic("state decode refused")}
 p,err:=shimcodex.BuildDeliveryProjection(s)
 out:=map[string]any{"revision":s.Revision,"inbox_count":len(s.Inbox),"projected_source_count":len(p.Sources),"private_active":s.ActiveTurn!=""}
 if err!=nil {if f,ok:=err.(*shimcodex.Failure);ok {out["refusal"]=f.Code}else {out["refusal"]="non_protocol_error"}}
 if len(p.Sources)>0 {
  last:=p.Sources[len(p.Sources)-1];out["last_source_kind"]=last.Kind;out["last_disposition"]=last.Disposition;out["cursor"]=last.Cursor
  for i,e:=range s.Inbox {if e.Identity!=last.SourceEventID {continue};out["inbox_index"]=i
   var envelope map[string]json.RawMessage;_ = json.Unmarshal(e.Raw,&envelope);out["envelope_keys"]=keys(envelope)
   var method string;_ = json.Unmarshal(envelope["method"],&method);out["method"]=method
   var params map[string]json.RawMessage;_ = json.Unmarshal(envelope["params"],&params);out["params_keys"]=keys(params)
   for _,name:=range []string{"item","turn"} {var obj map[string]json.RawMessage;if json.Unmarshal(params[name],&obj)==nil {out[name+"_keys"]=keys(obj);var typ string;_ = json.Unmarshal(obj["type"],&typ);out[name+"_type"]=typ;var status string;_ = json.Unmarshal(obj["status"],&status);if status!="" {out[name+"_status"]=status}}}
  }
 }
 _=json.NewEncoder(os.Stdout).Encode(out)
}
