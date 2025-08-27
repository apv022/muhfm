import os
import curses
import subprocess
import shutil
def nav_down(state):
    return {**state, 'index': min(state ['index'] + 1, len(state['files'])-1)}

def nav_up(state):
    return {**state, 'index': max(state ['index'] - 1, 0)}

def nav_parent(state):
    parent = os.path.dirname(state['current_dir'])
    if parent == state['current_dir']:
        return state
    return{
        **state,
        'current_dir': parent,
        'files': refresh(parent),
        'index': 0
        }
def select_file(state):
    selected = state['files'][state['index']]
    if selected == '..':
        return nav_parent(state)
    full_path = os.path.join(state['current_dir'],selected)
    if os.path.isdir(full_path):
        return {
            **state,
            'current_dir': full_path,
            'files': refresh(full_path),
            'index':0
            }
    else:
        open_file(full_path)
        return state

def open_file(path):
    try:
        subprocess.run(['xdg-open', path])
    except Exception:
        return f'Error: {str(Exception)}'

def refresh(directory, show_hidden=False):
    try:
        entries = list(os.scandir(directory))
        entries.sort(key=lambda e:( e.name.lower()))

        files = [
            entry.name for entry in entries
            if show_hidden or not entry.name.startswith('.')
        ]
        if directory != '/':
            files.insert(0,'..')
        return files
    except PermissionError:
        return ['[Permission Denied]']

def toggle_hidden(state):
    new_state = {
        **state,
        'show_hidden': not state.get('show_hidden', False)
    }
    return {
        **new_state,
        'files': refresh(new_state['current_dir'], new_state['show_hidden']),
    }
def copy_index(state):
    src = state['files'][state['index']]
    if src == '..':
        return state
    full_path = os.path.join(state['current_dir'], src)
    return {
        **state,
        'copied_file': full_path
    }

def paste_index(state):
    if not state['copied_file']:
        return state

    src = state['copied_file']
    dest = os.path.join(state['current_dir'], os.path.basename(src))
    if os.path.isdir(src):
        shutil.copytree(src, dest)
    else:
        shutil.copy2(src, dest)

    return {
        **state,
        'files': refresh(state['current_dir'])
        }

def delete_file(state):
    src = state['files'][state['index']]
    file_to_rm = os.path.join(state['current_dir'],src)
    if src== '..':
        return state
    elif os.path.isdir(file_to_rm):
        shutil.rmtree(file_to_rm)
    else:
        os.remove(file_to_rm)
    return {
        **state,
        'files': refresh(state['current_dir'])
        }
key_bindings={
        ord('j'): nav_down,
        ord('h'): nav_parent,
        ord('k'): nav_up,
        ord('l'): select_file,
        ord('.'): toggle_hidden,
        ord('q'): lambda state: {**state, 'running': False},
        ord('y'): copy_index,
        ord('p'): paste_index,
        ord('D'): delete_file
        }

def main(stdscr):
    curses.curs_set(0)
    curses.use_default_colors()
    state = {
        'current_dir': (current_dir:=os.getcwd()),
        'files': refresh(current_dir),
        'index': 0,
        'scroll_offset': 0,
        'show_hidden': False,
        'running': True,
        'copied_file': None
        }

    while state["running"]:
        stdscr.clear()
        height, width = stdscr.getmaxyx()

        if state['index'] < state['scroll_offset']:
            state['scroll_offset'] = state['index']
        elif state['index'] >= state['scroll_offset'] + height:
            state['scroll_offset'] = state['index'] - height +1

        visible_files = state['files'][state['scroll_offset']:state['scroll_offset'] + height]

        for i, item in enumerate(visible_files):
            file_index = state['scroll_offset'] + i
            attr = curses.A_REVERSE if file_index == state["index"] else curses.A_NORMAL
            try:
                stdscr.addstr(i, 0, item, attr)
            except curses.error:
                pass


        key = stdscr.getch()
        action = key_bindings.get(key, lambda s: s)
        state = action(state)

if __name__ == "__main__":
    curses.wrapper(main)

