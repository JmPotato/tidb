import json
from pathlib import Path
for path in [Path('pkg/metrics/grafana/tidb_resource_control.json'),Path('pkg/metrics/nextgengrafana/tidb_resource_control_with_keyspace_name.json')]:
 d=json.loads(path.read_text()); row=next(p for p in d['panels'] if p['title']=='Client');panels=row['panels']
 assert row['collapsed'] and len(panels)==8
 ordered=sorted(panels,key=lambda p:(p['gridPos']['y'],p['gridPos']['x']))
 assert [p['title'] for p in ordered]==[p['title'] for p in panels], 'Grafana sorts the collapsed Client panels out of their declared order'
 for i,a in enumerate(panels):
  a=a['gridPos']
  for b in panels[i+1:]:
   b=b['gridPos']
   overlap=a['x']<b['x']+b['w'] and b['x']<a['x']+a['w'] and a['y']<b['y']+b['h'] and b['y']<a['y']+a['h']
   assert not overlap, 'Client panels must not occupy overlapping grid cells'
 last=panels[-1]
 assert last['title']=='Client RU' and last['gridPos']['w']==24 and last['gridPos']['x']==0
 print(path, 'stable expansion order, paired rows, full-width Client RU at the end, no overlaps: PASS')
