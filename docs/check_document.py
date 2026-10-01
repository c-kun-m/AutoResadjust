from pathlib import Path
import re,json,sys
from pypdf import PdfReader
from PIL import Image,ImageDraw,ImageFont
ROOT=Path(__file__).resolve().parents[1]
QA=ROOT/'tmp/doc_qa'
RENDER=QA/(sys.argv[1] if len(sys.argv)>1 else 'render')
r=PdfReader(next(RENDER.glob('*.pdf')))
texts=[p.extract_text() for p in r.pages]
headings=re.findall(r'^## (.+)$',(ROOT/'docs/跨数据中心异构AI算力平台项目总览.md').read_text('utf-8'),re.M)
pm={}
def scan_outline(items):
    for item in items:
        if isinstance(item,list):scan_outline(item)
        elif str(item.title) in headings:
            pm[str(item.title)]=r.get_destination_page_number(item)+1
scan_outline(r.outline)
for i,t in enumerate(texts):
    if i<2:continue
    for h in headings:
        if h not in pm and re.sub(r'\s+','',h) in re.sub(r'\s+','',t):pm[h]=i+1
(QA/'page_map.json').write_text(json.dumps(pm,ensure_ascii=False,indent=2),'utf-8')
(QA/'extracted.txt').write_text('\n\n'.join('PAGE '+str(i+1)+'\n'+t for i,t in enumerate(texts)),'utf-8')
print('page_count',len(texts),'mapped_headings',len(pm))
print(json.dumps([{'page':i+1,'chars':len(t),'first':t[:65],'last':t[-70:]} for i,t in enumerate(texts)],ensure_ascii=False))
files=sorted(RENDER.glob('page-*.png'),key=lambda x:int(x.stem.split('-')[1]))
fnt=ImageFont.truetype('C:/Windows/Fonts/arial.ttf',20)
for j in range((len(files)+11)//12):
    sheet=Image.new('RGB',(1050,1900),'#dddddd')
    for k,f in enumerate(files[j*12:(j+1)*12]):
        im=Image.open(f).convert('RGB');im.thumbnail((330,440))
        x=(k%3)*350;y=(k//3)*475
        sheet.paste(im,(x,y+28))
        ImageDraw.Draw(sheet).text((x+8,y+4),'PAGE '+str(j*12+k+1),font=fnt,fill='black')
    sheet.save(QA/('contact'+str(j+1)+'.png'))
