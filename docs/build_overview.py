from pathlib import Path
import re, json
from PIL import Image, ImageDraw, ImageFont
from docx import Document
from docx.shared import Inches, Pt, RGBColor
from docx.enum.text import WD_ALIGN_PARAGRAPH, WD_BREAK, WD_TAB_ALIGNMENT, WD_TAB_LEADER
from docx.enum.table import WD_TABLE_ALIGNMENT, WD_CELL_VERTICAL_ALIGNMENT
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.opc.constants import RELATIONSHIP_TYPE as RT

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / 'docs/跨数据中心异构AI算力平台项目总览.md'
OUT = ROOT / 'output/跨数据中心异构AI算力平台项目总览.docx'
QA = ROOT / 'tmp/doc_qa'
FONT = 'Microsoft YaHei'

def diagram():
    img = Image.new('RGB', (2400, 1700), 'white')
    d = ImageDraw.Draw(img)
    f = ImageFont.truetype('C:/Windows/Fonts/msyh.ttc', 34)
    fb = ImageFont.truetype('C:/Windows/Fonts/msyhbd.ttc', 38)
    small = ImageFont.truetype('C:/Windows/Fonts/msyh.ttc', 29)
    def box(coords, title, lines=(), fill='#F2F6FA'):
        x0,y0,x1,y1=coords
        d.rounded_rectangle(coords, radius=10, fill=fill, outline='#8C9DAF', width=2)
        d.text(((x0+x1)/2,y0+24),title,font=fb,fill='#142A43',anchor='mt')
        for j,line in enumerate(lines):
            d.text(((x0+x1)/2,y0+80+j*46),line,font=f,fill='#222222',anchor='mt')
    def arrow(points,color='#526B84',width=5):
        d.line(points,fill=color,width=width)
        x,y=points[-1]; px,py=points[-2]
        if x==px:
            k=1 if y>py else -1; tri=[(x,y),(x-13,y-23*k),(x+13,y-23*k)]
        else:
            k=1 if x>px else -1; tri=[(x,y),(x-23*k,y-13),(x-23*k,y+13)]
        d.polygon(tri,fill=color)
    box((60,35,1830,195),'业务接入', ['控制台  /  API  /  SDK    ·    单点登录  权限  配额  预算'])
    box((60,265,1190,495),'全局控制面', ['任务与队列    全局放置    服务部署控制', '资源目录    模型与数据目录    策略版本'])
    box((1270,265,1830,495),'在线推理入口', ['请求鉴权  限流  路由', '只选择已就绪模型副本'])
    arrow([(610,195),(610,265)])
    arrow([(1550,195),(1550,265)],'#166C77')
    box((60,565,1190,760),'数据与状态基础', ['关系数据库  事件总线  对象存储', '资源预留  版本目录  传输与副本管理'])
    arrow([(610,495),(610,565)])
    box((60,890,890,1150),'机房 A 代理与本地控制', ['持久化指令  本地预留  状态核对', 'Kubernetes  成组准入  拓扑调度'])
    box((1000,890,1830,1150),'机房 B 代理与本地控制', ['持久化指令  本地预留  状态核对', 'Kubernetes  成组准入  拓扑调度'])
    arrow([(440,760),(440,890)])
    arrow([(940,760),(940,825),(1260,825),(1260,890)])
    box((60,1220,890,1450),'机房 A 执行资源', ['已认证 GPU 与高速网络', '训练与微调  批量推理  就绪服务'])
    box((1000,1220,1830,1450),'机房 B 执行资源', ['已认证昇腾 NPU 或其他资源', '训练与微调  批量推理  就绪服务'])
    arrow([(470,1150),(470,1220)])
    arrow([(1415,1150),(1415,1220)])
    arrow([(1690,495),(1900,495),(1900,1360),(1830,1360)],'#166C77',7)
    arrow([(1900,1360),(1900,1490),(935,1490),(935,1360),(890,1360)],'#166C77',7)
    d.text((1720,670),'请求数据流',font=small,fill='#166C77',anchor='mm')
    d.text((1720,716),'经专用网络到达',font=small,fill='#166C77',anchor='mm')
    d.text((1720,757),'各机房就绪服务',font=small,fill='#166C77',anchor='mm')
    box((1970,35,2340,1450),'横向能力', [], '#F5F5F5')
    for i,(a,b) in enumerate([('身份与安全','租户  密钥  审计'),('可观测性','指标  日志  链路'),('计量与账务','用量  对账  成本'),('平台工程','发布  备份  灾备')]):
        y=270+i*280
        d.text((2155,y),a,font=fb,fill='#142A43',anchor='mt')
        d.text((2155,y+66),b,font=small,fill='#333333',anchor='mt')
    d.text((60,1540),'控制与状态通过代理核对    ·    模型及数据通过受控通道分发',font=f,fill='#333333')
    d.text((60,1585),'示意资源类型可扩展    每个机房可以同时包含多个厂商资源池',font=small,fill='#555555')
    path = ROOT / 'docs/architecture.png'
    img.save(path)
    return path

def font(run,size=None,bold=None,color=None):
    run.font.name=FONT
    if size: run.font.size=Pt(size)
    if bold is not None: run.bold=bold
    run.font.color.rgb=RGBColor.from_string(color or '000000')
    rpr=run._element.get_or_add_rPr()
    rf=rpr.rFonts
    if rf is None:
        rf=OxmlElement('w:rFonts'); rpr.insert(0,rf)
    rf.set(qn('w:eastAsia'),FONT)
    for attr in ['asciiTheme','hAnsiTheme','eastAsiaTheme','cstheme','csTheme']:
        rf.attrib.pop(qn('w:'+attr),None)
    for attr in ['ascii','hAnsi','cs']:
        rf.set(qn('w:'+attr),FONT)

def add_link(p,label,url=None,anchor=None):
    h=OxmlElement('w:hyperlink')
    if anchor: h.set(qn('w:anchor'),anchor)
    else: h.set(qn('r:id'),p.part.relate_to(url,RT.HYPERLINK,is_external=True))
    r=OxmlElement('w:r'); rp=OxmlElement('w:rPr')
    rf=OxmlElement('w:rFonts'); rf.set(qn('w:ascii'),FONT);rf.set(qn('w:eastAsia'),FONT)
    rp.append(rf)
    c=OxmlElement('w:color');c.set(qn('w:val'),'244A70' if url else '000000');rp.append(c)
    r.append(rp);t=OxmlElement('w:t');t.text=label;r.append(t);h.append(r);p._p.append(h)

def bookmark(p,name,num):
    s=OxmlElement('w:bookmarkStart');s.set(qn('w:id'),str(num));s.set(qn('w:name'),name)
    e=OxmlElement('w:bookmarkEnd');e.set(qn('w:id'),str(num));p._p.insert(0,s);p._p.append(e)

def body(doc,text,style=None):
    p=doc.add_paragraph(style=style)
    # Human-readable references link to official bibliography entries.
    for chunk in re.split(r'(\[R\d{2}\])',text):
        if re.fullmatch(r'\[R\d{2}\]',chunk): add_link(p,chunk,anchor=chunk[1:-1])
        else: font(p.add_run(chunk))
    return p

def table(doc,rows):
    n=len(rows[0]);t=doc.add_table(rows=1,cols=n)
    t.alignment=WD_TABLE_ALIGNMENT.CENTER;t.autofit=False
    headers=rows[0]
    if n==2: widths=[1.30,5.70]
    elif n==3: widths=[1.35,2.90,2.75]
    else: widths=[1.30,1.30,2.25,2.15]
    if headers[0]=='编号': widths=([.62,1.13,3.55,1.70] if headers[1]=='对应范围' else [.55,1.05,3.83,1.57])
    elif headers[0]=='阶段': widths=[1.40,1.0,2.65,1.95]
    elif headers[0]=='风险': widths=[1.32,2.10,2.30,1.28]
    elif headers[0]=='接口': widths=[2.35,1.85,2.80]
    elif headers[0]=='工作包': widths=[1.55,1.20,1.35,2.90]
    elif headers[0]=='实体': widths=[1.48,2.75,2.77]
    elif headers[0]=='角色' and headers[1]=='建议投入':widths=[1.60,1.30,4.10]
    elif headers[0]=='指标': widths=[1.50,2.50,3.00]
    for c,w in zip(t.columns,widths):c.width=Inches(w)
    pr=t._tbl.tblPr
    borders=OxmlElement('w:tblBorders')
    for side in ['top','left','bottom','right','insideH','insideV']:
        e=OxmlElement('w:'+side);e.set(qn('w:val'),'single');e.set(qn('w:sz'),'4');e.set(qn('w:color'),'D9D9D9');borders.append(e)
    pr.append(borders)
    margins=OxmlElement('w:tblCellMar')
    for side,val in [('top',90),('bottom',90),('left',105),('right',105)]:
        e=OxmlElement('w:'+side);e.set(qn('w:w'),str(val));e.set(qn('w:type'),'dxa');margins.append(e)
    pr.append(margins)
    for i,rowdata in enumerate(rows):
        row=t.rows[0] if i==0 else t.add_row()
        trpr=row._tr.get_or_add_trPr()
        nosplit=OxmlElement('w:cantSplit');trpr.append(nosplit)
        if i==0:
            repeat=OxmlElement('w:tblHeader');trpr.append(repeat)
        for j,(cell,txt) in enumerate(zip(row.cells,rowdata)):
            cell.width=Inches(widths[j]);cell.vertical_alignment=WD_CELL_VERTICAL_ALIGNMENT.CENTER
            sh=OxmlElement('w:shd');sh.set(qn('w:fill'),'DCE6EF' if i==0 else ('F6F8FA' if i%2==0 else 'FFFFFF'));cell._tc.get_or_add_tcPr().append(sh)
            p=cell.paragraphs[0];p.paragraph_format.space_after=Pt(0);p.paragraph_format.space_before=Pt(0);p.paragraph_format.line_spacing=1.2
            if i<2:p.paragraph_format.keep_with_next=True
            if j==0 and headers[0]=='编号':p.alignment=WD_ALIGN_PARAGRAPH.CENTER
            font(p.add_run(txt),9.5 if n==4 else 10,bold=i==0)
    p=doc.add_paragraph();p.paragraph_format.space_after=Pt(3);p.paragraph_format.space_before=Pt(0);p.paragraph_format.line_spacing=Pt(3);font(p.add_run(''),3)
    return t

def main():
    art=diagram()
    doc=Document();sec=doc.sections[0]
    sec.page_width=Inches(8.5);sec.page_height=Inches(11)
    sec.left_margin=sec.right_margin=Inches(.75)
    sec.top_margin=Inches(.68);sec.bottom_margin=Inches(.68)
    sec.header_distance=Inches(.25);sec.footer_distance=Inches(.28)
    for name in ['Normal','Title','Subtitle','Heading 1','Heading 2','Heading 3']:
        st=doc.styles[name];st.font.name=FONT;st.font.color.rgb=RGBColor(0,0,0)
        rf=st._element.get_or_add_rPr().rFonts
        for attr in list(rf.attrib):
            if 'theme' in attr.lower():del rf.attrib[attr]
        for attr in ['ascii','hAnsi','eastAsia','cs']:rf.set(qn('w:'+attr),FONT)
        st.font.italic=False;st.font.underline=False
    for e in doc.styles.element.xpath('.//w:pBdr'):
        e.getparent().remove(e)
    st=doc.styles['Normal'];st.font.size=Pt(11);st.paragraph_format.line_spacing=1.35
    st.paragraph_format.space_after=Pt(6);st.paragraph_format.widow_control=True
    for name,size,bef,aft in [('Title',25,0,14),('Subtitle',16,0,12),('Heading 1',17,16,10),('Heading 2',13,12,7)]:
        st=doc.styles[name];st.font.size=Pt(size);st.paragraph_format.space_before=Pt(bef);st.paragraph_format.space_after=Pt(aft)
        st.paragraph_format.keep_with_next=True
    sec.different_first_page_header_footer=True
    h=sec.header.paragraphs[0];font(h.add_run('跨数据中心异构 AI 算力管理与调度平台'),8)
    h.paragraph_format.space_after=Pt(0)
    footer=sec.footer.paragraphs[0];footer.alignment=WD_ALIGN_PARAGRAPH.RIGHT
    font(footer.add_run('项目总览  |  1.0     '),8)
    field=OxmlElement('w:fldSimple');field.set(qn('w:instr'),'PAGE');footer._p.append(field)
    doc.core_properties.title='跨数据中心异构 AI 算力管理与调度平台项目总览'
    doc.core_properties.subject='工业级项目总览与建设规划'
    doc.core_properties.author='项目规划'
    doc.core_properties.keywords='异构算力 跨数据中心 GPU 昇腾 调度 项目规划'
    title=doc.add_paragraph(style='Title');font(title.add_run('跨数据中心异构 AI 算力\n管理与调度平台'),25,bold=True)
    p=doc.add_paragraph('工业级项目总览与建设规划',style='Subtitle')
    body(doc,'版本 1.0  |  2026 年 9 月 18 日')
    body(doc,'适用阶段  立项与总体设计评审')
    p=doc.add_paragraph();p.paragraph_format.space_after=Pt(14)
    doc.add_heading('建设目标',level=1)
    body(doc,'统一管理多个数据中心的 GPU、昇腾 NPU 与经认证的其他算力，为训练、微调、批量推理及在线推理提供可长期生产运行的资源、调度、数据和运营能力。')
    body(doc,'本规划覆盖完整产品范围、分层架构、异构适配、可靠性、安全治理、计量对账、实施组织和验收体系，用于形成可评审的立项与建设基线。')
    doc.add_heading('规划要点',level=1)
    for s in ['统一控制面与机房自治执行面，训练放置与在线请求路由分别设计。','以兼容矩阵、拓扑约束、幂等与状态核对保障生产执行。','建议约 40 周完成建设、联合验收及稳定运行观察，投入参考为 180 至 240 人月。','覆盖 11 个功能模块、9 个工作包与 22 项关键验收场景，规模与服务等级在基线评审中冻结。']:
        body(doc,'• '+s)
    body(doc,'文档对象  项目决策人、产品与架构负责人、研发团队、SRE、安全、运营及验收人员。')
    doc.add_page_break()
    doc.add_heading('目录',level=1)
    text=SRC.read_text(encoding='utf-8')
    headings=re.findall(r'^## (.+)$',text,re.M)
    page_map=json.loads((QA/'page_map.json').read_text('utf-8')) if (QA/'page_map.json').exists() else {}
    for idx,title in enumerate(headings):
        p=doc.add_paragraph();p.paragraph_format.space_after=Pt(3);p.paragraph_format.line_spacing=1.08
        p.paragraph_format.tab_stops.add_tab_stop(Inches(6.98),WD_TAB_ALIGNMENT.RIGHT,WD_TAB_LEADER.DOTS)
        add_link(p,title,anchor='s'+str(idx));font(p.add_run('\t'+str(page_map.get(title,''))),10.5)
    doc.add_page_break()
    lines=text.splitlines();start=next(i for i,l in enumerate(lines) if l.startswith('## '));i=start;idx=0
    major={'5'}
    while i<len(lines):
        l=lines[i].strip()
        if not l:i+=1;continue
        if l.startswith('## '):
            title=l[3:];p=doc.add_heading(title,level=1);bookmark(p,'s'+str(idx),idx+1);idx+=1
            if title.split()[0] in major or title.startswith('附录 '):p.paragraph_format.page_break_before=True
        elif l.startswith('### '):doc.add_heading(re.sub(r'^\d+\.\d+\s+','',l[4:]),level=2)
        elif l.startswith('|'):
            rows=[]
            while i<len(lines) and lines[i].strip().startswith('|'):
                r=[c.strip() for c in lines[i].strip().strip('|').split('|')]
                if not all(re.fullmatch(r'[:\- ]+',c) for c in r):rows.append(r)
                i+=1
            table(doc,rows);continue
        elif l.startswith('!['):
            p=doc.add_paragraph();p.alignment=WD_ALIGN_PARAGRAPH.CENTER
            p.add_run().add_picture(str(art),width=Inches(7))
            p.paragraph_format.keep_with_next=True
            c=doc.add_paragraph();c.alignment=WD_ALIGN_PARAGRAPH.CENTER;font(c.add_run('图 1  平台分层架构与主要链路'),9.5)
            inline=p._p.xpath('.//wp:docPr')[0];inline.set('descr','统一业务接入连接全局控制面和在线推理入口，经机房代理控制本地异构执行资源，安全观测计量贯穿各层。')
        elif l.startswith('```'):
            i+=1;code=[]
            while i<len(lines) and not lines[i].startswith('```'):code.append(lines[i]);i+=1
            for k,line in enumerate(code):
                p=doc.add_paragraph();p.paragraph_format.space_after=Pt(0);p.paragraph_format.line_spacing=1.03
                p.paragraph_format.keep_with_next=k<len(code)-1
                p.paragraph_format.left_indent=Inches(.12)
                r=p.add_run(line);font(r,9);r.font.name='Consolas'
        elif l.startswith('https://'):
            p=doc.add_paragraph();p.paragraph_format.space_after=Pt(7);p.paragraph_format.line_spacing=1.05;add_link(p,l,url=l)
            for rr in p._p.xpath('.//w:rPr'):
                sz=OxmlElement('w:sz');sz.set(qn('w:val'),'18');rr.append(sz)
        else:
            p=body(doc,l)
            if re.match(r'^\[R\d{2}\]',l):
                ref=l[1:4];bookmark(p,ref,100+int(ref[1:]));p.paragraph_format.keep_with_next=True
                p.paragraph_format.space_after=Pt(2);p.paragraph_format.line_spacing=1.1
                for rr in p.runs:font(rr,10)
        i+=1
    for e in doc.element.xpath('.//w:pBdr'):
        e.getparent().remove(e)
    for p in doc.paragraphs:
        if p.style.name.startswith('Heading') or p.style.name in ['Title','Subtitle']:
            size={'Title':25,'Subtitle':16,'Heading 1':17,'Heading 2':13}.get(p.style.name,12)
            for r in p.runs:
                font(r,size,bold=p.style.name!='Subtitle')
                r.font.italic=False
    OUT.parent.mkdir(exist_ok=True,parents=True);doc.save(OUT)
    print(str(OUT));print('body_chars',len(text),'headings',len(headings))

if __name__=='__main__': main()
