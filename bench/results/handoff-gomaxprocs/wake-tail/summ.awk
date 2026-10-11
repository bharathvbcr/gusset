# Per variant: runs, FAILs, median range, share over 200us and over 1ms (min-max), from probe lines.
/\|/ {
  v=$1; st=($0 ~ /--- FAIL/) ? "F" : "P"
  match($0,/ n=[0-9]+/); n=substr($0,RSTART+3,RLENGTH-3)+0
  match($0,/p50=[^ ]+/); p=substr($0,RSTART+4,RLENGTH-4)
  us = (p ~ /ms$/) ? p*1000 : (p ~ /µs$/) ? p+0 : (p ~ /ns$/) ? p/1000 : p*1e6
  match($0,/over200us=[0-9]+/); a=substr($0,RSTART+10,RLENGTH-10)/n
  match($0,/over1ms=[0-9]+/); b=substr($0,RSTART+8,RLENGTH-8)/n
  runs[v]++; if (st=="F") fails[v]++
  if (!(v in pmin) || us<pmin[v]) pmin[v]=us; if (us>pmax[v]) pmax[v]=us
  if (!(v in amin) || a<amin[v]) amin[v]=a; if (a>amax[v]) amax[v]=a
  if (!(v in bmin) || b<bmin[v]) bmin[v]=b; if (b>bmax[v]) bmax[v]=b
}
END { for (v in runs) printf "%-14s fail %2d/%2d  median %8.1f-%8.1f us  >200us %5.1f-%5.1f%%  >1ms %5.1f-%5.1f%%\n", v, fails[v]+0, runs[v], pmin[v], pmax[v], amin[v]*100, amax[v]*100, bmin[v]*100, bmax[v]*100 }
